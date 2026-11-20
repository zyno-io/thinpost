import { describe, expect, it } from 'vitest';

import { readDeliveryRecipient, readDeliveryRecipientKind, readMessageHold, readMessageSummary } from '@/message-summary';

const ID = '0123456789abcdef0123456789abcdef';
function raw(overrides: Record<string, unknown> = {}) {
    return {
        id: ID,
        state: 'prepared',
        status: 'delivered',
        enhanced_protection: false,
        subject_masked: false,
        accepted_at: '2026-10-02T12:00:00Z',
        delivery_count: 2,
        accepted_count: 1,
        failed_count: 0,
        uncertain_count: 0,
        ...overrides
    };
}

describe('readMessageSummary', () => {
    it('reads the console test tag only when it is well formed', () => {
        expect(readMessageSummary(raw())?.operatorTest).toBeNull();
        expect(readMessageSummary(raw({ operator_test: { bypass_limits: true, bypass_recipient_policy: false } }))?.operatorTest).toEqual({ bypassLimits: true, bypassRecipientPolicy: false });
        for (const bad of [null, 'yes', {}, { bypass_limits: true }, { bypass_limits: 'true', bypass_recipient_policy: false }]) {
            expect(readMessageSummary(raw({ operator_test: bad }))?.operatorTest).toBeNull();
        }
    });

    it('requires a status', () => {
        expect(readMessageSummary(raw({ status: 'sending' }))?.status).toBe('sending');
        expect(readMessageSummary(raw({ status: undefined }))).toBeNull();
    });

    it('reads a plain message', () => {
        const row = readMessageSummary(raw({ subject: 'Hi', recipients: ['a@x.example', 'b@x.example'], recipient_count: 2, sender_domain: 'Example.COM' }));
        expect(row).toMatchObject({
            id: ID,
            enhancedProtection: false,
            subject: { kind: 'text', text: 'Hi' },
            recipients: { kind: 'list', shown: ['a@x.example', 'b@x.example'], total: 2 },
            senderDomain: 'example.com'
        });
    });

    it('drops the subject of an Enhanced Protection message even when sent', () => {
        const row = readMessageSummary(raw({ enhanced_protection: true, subject: 'secret' }));
        expect(row?.subject).toEqual({ kind: 'protected' });
        expect(JSON.stringify(row)).not.toContain('secret');
    });

    it('treats subject_masked as protected', () => {
        expect(readMessageSummary(raw({ subject_masked: true, subject: 'secret' }))?.subject).toEqual({ kind: 'protected' });
    });

    it('requires the protection flags', () => {
        const { enhanced_protection: _a, ...noEp } = raw();
        const { subject_masked: _b, ...noMask } = raw();
        expect(readMessageSummary(noEp)).toBeNull();
        expect(readMessageSummary(noMask)).toBeNull();
    });

    it('marks a missing subject and non-string recipients as not recorded', () => {
        const row = readMessageSummary(raw({ recipients: [1, 2], recipient_count: 2 }));
        expect(row?.subject).toEqual({ kind: 'not-recorded' });
        expect(row?.recipients).toEqual({ kind: 'not-recorded', total: 2 });
    });

    it('rejects negative, fractional and non-numeric counts', () => {
        expect(readMessageSummary(raw({ delivery_count: -1 }))).toBeNull();
        expect(readMessageSummary(raw({ accepted_count: 1.5 }))).toBeNull();
        expect(readMessageSummary(raw({ failed_count: '1' }))).toBeNull();
        expect(readMessageSummary(raw({ id: 'nope' }))).toBeNull();
        expect(readMessageSummary(null)).toBeNull();
    });

    it('ignores an invalid recipient_count', () => {
        expect(readMessageSummary(raw({ recipients: ['a@x.example'], recipient_count: -3 }))?.recipients).toEqual({
            kind: 'list',
            shown: ['a@x.example'],
            kinds: ['to'],
            total: null
        });
    });

    it('reads recipient kinds, and treats missing or malformed kinds as To', () => {
        const recipients = ['a@x.example', 'b@x.example', 'c@x.example'];
        expect(readMessageSummary(raw({ recipients, recipient_kinds: ['to', 'cc', 'bcc'] }))?.recipients).toMatchObject({ kinds: ['to', 'cc', 'bcc'] });
        expect(readMessageSummary(raw({ recipients, recipient_kinds: ['to', 'cc'] }))?.recipients).toMatchObject({ kinds: ['to', 'to', 'to'] });
        expect(readMessageSummary(raw({ recipients, recipient_kinds: ['to', 'cc', 'reply'] }))?.recipients).toMatchObject({ kinds: ['to', 'to', 'to'] });
        expect(readMessageSummary(raw({ recipients }))?.recipients).toMatchObject({ kinds: ['to', 'to', 'to'] });
        expect(readMessageSummary(raw({ recipients, recipient_kinds: ['to', 'cc', 'bcc'] }), { maxRecipients: 2 })?.recipients).toMatchObject({ shown: ['a@x.example', 'b@x.example'], kinds: ['to', 'cc'] });
    });

    it('caps shown recipients and does not retain unknown fields', () => {
        const row = readMessageSummary(raw({ recipients: ['1@x.example', '2@x.example', '3@x.example', '4@x.example'], extra: 'x' }));
        expect(row?.recipients).toMatchObject({ shown: ['1@x.example', '2@x.example', '3@x.example'] });
        expect(readMessageSummary(raw({ recipients: ['1@x.example', '2@x.example', '3@x.example', '4@x.example'] }), { maxRecipients: Infinity })?.recipients).toMatchObject({
            shown: ['1@x.example', '2@x.example', '3@x.example', '4@x.example']
        });
        expect(Object.keys(row!)).not.toContain('extra');
    });

    it('keeps the sender domain for protected messages and nulls absent or empty ones', () => {
        expect(readMessageSummary(raw({ enhanced_protection: true, sender_domain: 'Mail.Example.com' }))?.senderDomain).toBe('mail.example.com');
        expect(readMessageSummary(raw())?.senderDomain).toBeNull();
        expect(readMessageSummary(raw({ sender_domain: '' }))?.senderDomain).toBeNull();
    });

    it('reads the sender, with or without a name, and nulls absent or malformed ones', () => {
        expect(readMessageSummary(raw({ enhanced_protection: true, sender: { address: 'A@x.example', name: 'A B' } }))?.sender).toEqual({ address: 'A@x.example', name: 'A B' });
        expect(readMessageSummary(raw({ sender: { address: 'A@x.example' } }))?.sender).toEqual({ address: 'A@x.example', name: null });
        expect(readMessageSummary(raw())?.sender).toBeNull();
        expect(readMessageSummary(raw({ sender: { name: 'only a name' } }))?.sender).toBeNull();
    });
});

describe('readDeliveryRecipient', () => {
    it('returns a recorded address only', () => {
        expect(readDeliveryRecipient({ recipient: 'a@x.example' })).toBe('a@x.example');
        expect(readDeliveryRecipient({})).toBeNull();
        expect(readDeliveryRecipient({ recipient: '' })).toBeNull();
        expect(readDeliveryRecipient({ recipient: 4 })).toBeNull();
        expect(readDeliveryRecipient(undefined)).toBeNull();
    });
});

describe('readDeliveryRecipientKind', () => {
    it('returns a known kind only', () => {
        expect(readDeliveryRecipientKind({ recipient_kind: 'cc' })).toBe('cc');
        expect(readDeliveryRecipientKind({ recipient_kind: 'bcc' })).toBe('bcc');
        expect(readDeliveryRecipientKind({ recipient_kind: 'to' })).toBe('to');
        expect(readDeliveryRecipientKind({ recipient_kind: 'reply' })).toBeNull();
        expect(readDeliveryRecipientKind({})).toBeNull();
        expect(readDeliveryRecipientKind(undefined)).toBeNull();
    });
});

describe('readMessageHold', () => {
    it('is null for a message not submitted on hold', () => {
        expect(readMessageHold(undefined)).toBeNull();
        expect(readMessageHold(null)).toBeNull();
    });

    it('reads an open and a resolved hold', () => {
        expect(readMessageHold({ expires_at: '2026-11-25T09:00:00Z' })).toEqual({ expiresAt: '2026-11-25T09:00:00Z', resolved: null });
        for (const resolution of ['released', 'cancelled', 'expired'] as const) {
            expect(readMessageHold({ expires_at: '2026-11-25T09:00:00Z', resolution, resolved_at: '2026-11-19T09:00:00Z' })).toEqual({
                expiresAt: '2026-11-25T09:00:00Z',
                resolved: { resolution, time: '2026-11-19T09:00:00Z' }
            });
        }
    });

    it('rejects malformed holds', () => {
        expect(readMessageHold('held')).toBeUndefined();
        expect(readMessageHold({})).toBeUndefined();
        expect(readMessageHold({ expires_at: '2026-11-25T09:00:00Z', resolution: 'paused', resolved_at: '2026-11-19T09:00:00Z' })).toBeUndefined();
        expect(readMessageHold({ expires_at: '2026-11-25T09:00:00Z', resolution: 'released' })).toBeUndefined();
        expect(readMessageHold({ expires_at: '2026-11-25T09:00:00Z', resolved_at: '2026-11-19T09:00:00Z' })).toBeUndefined();
    });
});
