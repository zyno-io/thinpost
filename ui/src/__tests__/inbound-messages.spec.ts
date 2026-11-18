import { describe, expect, it } from 'vitest';

import { InvalidResponseError } from '@/api-errors';
import { authResult, readInboundDetail, readInboundSummary, WEBHOOK_SUMMARY } from '@/inbound-messages';

const ID = '11111111111111111111111111111111';
const OUTBOUND = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
const DELIVERY = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb';

function summary(overrides: Record<string, unknown> = {}) {
    return {
        id: ID,
        received_at: '2026-11-13T10:00:00Z',
        from: { address: 'alice@example.org' },
        recipient: 'reply@mail.example.com',
        recipient_count: 2,
        subject: 'Re: hello',
        enhanced_protection: false,
        size_bytes: 2048,
        spf: 'pass',
        dkim: 'pass',
        dmarc: 'pass',
        correlation: { matched: false },
        webhook: { status: 'delivered', total: 2, delivered: 2, pending: 0, failed: 0 },
        content_expired: false,
        ...overrides
    };
}

function detail(overrides: Record<string, unknown> = {}) {
    return {
        message: summary(),
        content_expires_at: '2026-12-13T10:00:00Z',
        message_id_header: '<m@example.org>',
        envelope: { mail_from: 'bounce@example.org', recipients: ['reply@mail.example.com'], client_ip: '192.0.2.7', helo: 'mx.example.org', tls_version: 'TLS 1.3', tls_cipher: 'TLS_AES_128_GCM_SHA256' },
        authentication: {
            spf: { result: 'pass', domain: 'example.org', identity: 'mailfrom' },
            dkim: [{ result: 'pass', domain: 'example.org' }],
            dmarc: { result: 'pass', domain: 'example.org', policy: 'reject' },
            arc: { present: false, evaluated: false }
        },
        correlation: { matched: false },
        ...overrides
    };
}

describe('readInboundSummary', () => {
    it('keeps the contract fields', () => {
        const row = readInboundSummary(summary({ correlation: { matched: true, message_id: OUTBOUND } }));
        expect(row).toMatchObject({ id: ID, from: 'alice@example.org', recipient: 'reply@mail.example.com', recipientCount: 2, subject: 'Re: hello', sizeBytes: 2048, correlated: true, correlatedMessageId: OUTBOUND });
        expect(row?.webhook.status).toBe('delivered');
    });

    it('never keeps a subject for an Enhanced Protection message, even if the server sent one', () => {
        expect(readInboundSummary(summary({ enhanced_protection: true, subject: 'secret' }))?.subject).toBeNull();
    });

    it('rejects malformed rows', () => {
        for (const bad of [
            null,
            summary({ id: 'nope' }),
            summary({ recipient_count: -1 }),
            summary({ webhook: { status: 'weird', total: 0, delivered: 0, pending: 0, failed: 0 } }),
            summary({ correlation: undefined }),
            summary({ correlation: { matched: true, message_id: 'x' } }),
            summary({ content_expired: 'no' })
        ]) {
            expect(readInboundSummary(bad)).toBeNull();
        }
    });

    it('allows absent optional fields', () => {
        const row = readInboundSummary(summary({ from: undefined, recipient: undefined, subject: undefined }));
        expect(row).toMatchObject({ from: null, recipient: null, subject: null });
    });
});

describe('readInboundDetail', () => {
    it('reads the envelope, authentication and correlation', () => {
        const view = readInboundDetail(
            detail({ correlation: { matched: true, message_id: OUTBOUND, delivery_id: DELIVERY, header: 'references', from_matches_recipient: true } })
        );
        expect(view.envelope).toMatchObject({ mailFrom: 'bounce@example.org', clientIp: '192.0.2.7', helo: 'mx.example.org', tls: 'TLS 1.3 · TLS_AES_128_GCM_SHA256' });
        expect(view.authentication.dmarc.policy).toBe('reject');
        expect(view.authentication.arcPresent).toBe(false);
        expect(view.correlation).toEqual({ matched: true, messageId: OUTBOUND, deliveryId: DELIVERY, header: 'references', fromMatchesRecipient: true });
    });

    it('says nothing about encryption when TLS was not recorded, and leaves the null sender empty', () => {
        const view = readInboundDetail(detail({ envelope: { recipients: [] } }));
        expect(view.envelope.tls).toBeNull();
        expect(view.envelope.mailFrom).toBeNull();
    });

    it('rejects a response that does not match', () => {
        expect(() => readInboundDetail(detail({ authentication: { spf: { result: 'pass' }, dkim: 'x', dmarc: { result: 'pass' }, arc: { present: false } } }))).toThrow(InvalidResponseError);
        expect(() => readInboundDetail(detail({ correlation: { matched: true, header: 'subject' } }))).toThrow(InvalidResponseError);
        expect(() => readInboundDetail(null)).toThrow(InvalidResponseError);
    });
});

describe('labels', () => {
    it('describes every webhook summary state and falls back to the raw auth value', () => {
        for (const status of ['delivered', 'pending', 'failed', 'partial', 'none'] as const) expect(WEBHOOK_SUMMARY[status].label).toBeTruthy();
        expect(authResult('pass').tone).toBe('success');
        expect(authResult('something_new')).toMatchObject({ label: 'something_new', tone: '' });
    });
});
