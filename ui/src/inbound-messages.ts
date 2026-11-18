import { InvalidResponseError } from './api-errors';
import type { InboundMessageDetail, InboundMessageSummary } from './openapi-client-generated';
import { isOpaqueId } from './validation';

export type WebhookSummaryStatus = InboundMessageSummary['webhook']['status'];

export interface BadgeLabel {
    label: string;
    tone: 'success' | 'warning' | 'danger' | 'info' | '';
    description: string;
}

/** How the webhook deliveries of one received message are going. */
export const WEBHOOK_SUMMARY: Record<WebhookSummaryStatus, BadgeLabel> = {
    delivered: { label: 'Delivered', tone: 'success', description: 'Your webhook accepted this message.' },
    pending: { label: 'Waiting', tone: '', description: 'At least one send to your webhook is still waiting or being retried.' },
    failed: { label: 'Failed', tone: 'danger', description: 'Your webhook did not accept this message. You can send it again from the message page.' },
    partial: { label: 'Partly delivered', tone: 'warning', description: 'Your webhook accepted some sends of this message and not others. Open it to send the rest again.' },
    none: { label: 'No webhook send', tone: '', description: 'Nothing was queued for a webhook.' }
};

const AUTH_RESULTS: Record<string, BadgeLabel> = {
    pass: { label: 'Pass', tone: 'success', description: 'The check passed.' },
    fail: { label: 'Fail', tone: 'danger', description: 'The check failed.' },
    softfail: { label: 'Soft fail', tone: 'warning', description: 'The check failed, but the domain asks receivers to treat that as a weak signal.' },
    neutral: { label: 'Neutral', tone: '', description: 'The domain makes no claim either way.' },
    none: { label: 'None', tone: '', description: 'Nothing to check: the domain publishes no record, or the message carries no signature.' },
    temperror: { label: 'Temporary error', tone: 'warning', description: 'The check could not finish in time (a DNS problem, for example). It says nothing about the sender.' },
    permerror: { label: 'Record error', tone: 'warning', description: "The domain's record is broken, so the check could not run." }
};

export function authResult(result: string): BadgeLabel {
    return (Object.hasOwn(AUTH_RESULTS, result) && AUTH_RESULTS[result]) || { label: result, tone: '', description: '' };
}

const isCount = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
const isString = (value: unknown, max = 1024): value is string => typeof value === 'string' && value.length <= max;

export interface InboundRow {
    id: string;
    receivedAt: string;
    /** The recorded header From address; the display name is not recorded. */
    from: string | null;
    recipient: string | null;
    recipientCount: number;
    /** Null for Enhanced Protection (masked), and when the message has none. */
    subject: string | null;
    enhancedProtection: boolean;
    sizeBytes: number;
    spf: string;
    dkim: string;
    dmarc: string;
    correlated: boolean;
    correlatedMessageId: string | null;
    webhook: { status: WebhookSummaryStatus; total: number; delivered: number; pending: number; failed: number };
    contentExpired: boolean;
}

/** Validates one list row; null when it does not match the contract. A subject is never kept for Enhanced Protection. */
export function readInboundSummary(raw: unknown): InboundRow | null {
    if (!raw || typeof raw !== 'object') return null;
    const v = raw as Record<string, unknown>;
    const w = (v.webhook ?? null) as Record<string, unknown> | null;
    const c = (v.correlation ?? null) as Record<string, unknown> | null;
    const from = (v.from ?? null) as Record<string, unknown> | null;
    if (
        !isOpaqueId(v.id) ||
        !isString(v.received_at, 64) ||
        !isCount(v.recipient_count) ||
        typeof v.enhanced_protection !== 'boolean' ||
        !isCount(v.size_bytes) ||
        !isString(v.spf, 32) ||
        !isString(v.dkim, 32) ||
        !isString(v.dmarc, 32) ||
        typeof v.content_expired !== 'boolean' ||
        !c ||
        typeof c.matched !== 'boolean' ||
        (c.message_id !== undefined && !isOpaqueId(c.message_id)) ||
        !w ||
        !(typeof w.status === 'string' && Object.hasOwn(WEBHOOK_SUMMARY, w.status)) ||
        !isCount(w.total) ||
        !isCount(w.delivered) ||
        !isCount(w.pending) ||
        !isCount(w.failed) ||
        (v.recipient !== undefined && !isString(v.recipient, 320)) ||
        (v.subject !== undefined && !isString(v.subject, 1024)) ||
        (from !== null && !isString(from.address, 320))
    ) {
        return null;
    }
    return {
        id: v.id,
        receivedAt: v.received_at,
        from: from ? (from.address as string) : null,
        recipient: (v.recipient as string | undefined) || null,
        recipientCount: v.recipient_count,
        subject: v.enhanced_protection ? null : ((v.subject as string | undefined) ?? null),
        enhancedProtection: v.enhanced_protection,
        sizeBytes: v.size_bytes,
        spf: v.spf,
        dkim: v.dkim,
        dmarc: v.dmarc,
        correlated: c.matched,
        correlatedMessageId: (c.message_id as string | undefined) ?? null,
        webhook: { status: w.status as WebhookSummaryStatus, total: w.total, delivered: w.delivered, pending: w.pending, failed: w.failed },
        contentExpired: v.content_expired
    };
}

export interface InboundAuthMethodView {
    result: string;
    domain: string | null;
    identity: string | null;
    policy: string | null;
}

export interface InboundDetailView {
    row: InboundRow;
    contentExpiresAt: string;
    envelope: {
        mailFrom: string | null;
        recipients: string[];
        clientIp: string | null;
        helo: string | null;
        tls: string | null;
    };
    messageIdHeader: string | null;
    authentication: { spf: InboundAuthMethodView; dkim: InboundAuthMethodView[]; dmarc: InboundAuthMethodView; arcPresent: boolean };
    correlation: {
        matched: boolean;
        messageId: string | null;
        deliveryId: string | null;
        header: 'in_reply_to' | 'references' | null;
        fromMatchesRecipient: boolean | null;
    };
}

function readMethod(raw: unknown): InboundAuthMethodView {
    const m = raw as Record<string, unknown> | null;
    if (!m || !isString(m.result, 32) || (m.domain !== undefined && !isString(m.domain, 255)) || (m.identity !== undefined && !isString(m.identity, 16)) || (m.policy !== undefined && !isString(m.policy, 32))) {
        throw new InvalidResponseError();
    }
    return { result: m.result, domain: (m.domain as string | undefined) || null, identity: (m.identity as string | undefined) || null, policy: (m.policy as string | undefined) || null };
}

/** Validates the detail response, throwing InvalidResponseError when it does not match the contract. */
export function readInboundDetail(raw: InboundMessageDetail | unknown): InboundDetailView {
    const v = raw as Record<string, unknown> | null;
    const row = readInboundSummary(v?.message);
    const e = (v?.envelope ?? null) as Record<string, unknown> | null;
    const a = (v?.authentication ?? null) as Record<string, unknown> | null;
    const c = (v?.correlation ?? null) as Record<string, unknown> | null;
    if (
        !v ||
        !row ||
        !isString(v.content_expires_at, 64) ||
        !e ||
        !Array.isArray(e.recipients) ||
        !e.recipients.every(r => isString(r, 320)) ||
        !a ||
        !Array.isArray(a.dkim) ||
        !a.arc ||
        typeof (a.arc as Record<string, unknown>).present !== 'boolean' ||
        !c ||
        typeof c.matched !== 'boolean' ||
        (c.message_id !== undefined && !isOpaqueId(c.message_id)) ||
        (c.delivery_id !== undefined && !isOpaqueId(c.delivery_id)) ||
        (c.header !== undefined && c.header !== 'in_reply_to' && c.header !== 'references') ||
        (c.from_matches_recipient !== undefined && typeof c.from_matches_recipient !== 'boolean') ||
        (v.message_id_header !== undefined && !isString(v.message_id_header, 998)) ||
        (e.mail_from !== undefined && !isString(e.mail_from, 320)) ||
        (e.client_ip !== undefined && !isString(e.client_ip, 64)) ||
        (e.helo !== undefined && !isString(e.helo, 255)) ||
        (e.tls_version !== undefined && !isString(e.tls_version, 32)) ||
        (e.tls_cipher !== undefined && !isString(e.tls_cipher, 128))
    ) {
        throw new InvalidResponseError();
    }
    const tlsVersion = e.tls_version as string | undefined;
    const tlsCipher = e.tls_cipher as string | undefined;
    return {
        row,
        contentExpiresAt: v.content_expires_at,
        envelope: {
            mailFrom: (e.mail_from as string | undefined) || null,
            recipients: e.recipients as string[],
            clientIp: (e.client_ip as string | undefined) || null,
            helo: (e.helo as string | undefined) || null,
            tls: tlsVersion ? (tlsCipher ? `${tlsVersion} · ${tlsCipher}` : tlsVersion) : null
        },
        messageIdHeader: (v.message_id_header as string | undefined) || null,
        authentication: { spf: readMethod(a.spf), dkim: (a.dkim as unknown[]).map(readMethod), dmarc: readMethod(a.dmarc), arcPresent: (a.arc as { present: boolean }).present },
        correlation: {
            matched: c.matched,
            messageId: (c.message_id as string | undefined) ?? null,
            deliveryId: (c.delivery_id as string | undefined) ?? null,
            header: (c.header as 'in_reply_to' | 'references' | undefined) ?? null,
            fromMatchesRecipient: (c.from_matches_recipient as boolean | undefined) ?? null
        }
    };
}
