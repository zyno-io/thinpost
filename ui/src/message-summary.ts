import { isOpaqueId } from '@/validation';

export type SubjectView = { kind: 'text'; text: string } | { kind: 'protected' } | { kind: 'not-recorded' };
export type RecipientKind = 'to' | 'cc' | 'bcc';
/** `kinds` is parallel to `shown`; older servers send no kinds, so every address is then a To address. */
export type RecipientsView = { kind: 'list'; shown: string[]; kinds: RecipientKind[]; total: number | null } | { kind: 'not-recorded'; total: number | null };

export interface MessageRow {
    id: string;
    state: string;
    /** Overall status derived by the server from the preparation state and delivery states. */
    status: string;
    enhancedProtection: boolean;
    acceptedAt: string;
    deliveryCount: number;
    acceptedCount: number;
    failedCount: number;
    uncertainCount: number;
    subject: SubjectView;
    recipients: RecipientsView;
    /** Never masked; null when not recorded. */
    senderDomain: string | null;
    /** From address and display name; never masked; null when not recorded. */
    sender: { address: string; name: string | null } | null;
}

const DEFAULT_MAX_RECIPIENTS = 3;

function isRecipientKind(value: unknown): value is RecipientKind {
    return value === 'to' || value === 'cc' || value === 'bcc';
}

function isCount(value: unknown): value is number {
    return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

/**
 * Validates one message summary from the operator API and keeps only contract fields. Fails closed for
 * Enhanced Protection: a subject is never retained when the message is protected or its subject is masked.
 * Returns null when a required field is missing or malformed.
 */
export function readMessageSummary(raw: unknown, options: { maxRecipients?: number } = {}): MessageRow | null {
    if (!raw || typeof raw !== 'object') return null;
    const v = raw as Record<string, unknown>;
    if (
        !isOpaqueId(v.id) ||
        typeof v.state !== 'string' ||
        typeof v.status !== 'string' ||
        typeof v.enhanced_protection !== 'boolean' ||
        typeof v.subject_masked !== 'boolean' ||
        typeof v.accepted_at !== 'string' ||
        !isCount(v.delivery_count) ||
        !isCount(v.accepted_count) ||
        !isCount(v.failed_count) ||
        !isCount(v.uncertain_count)
    ) {
        return null;
    }

    let subject: SubjectView;
    if (v.enhanced_protection || v.subject_masked) subject = { kind: 'protected' };
    else if (typeof v.subject === 'string') subject = { kind: 'text', text: v.subject };
    else subject = { kind: 'not-recorded' };

    const total = isCount(v.recipient_count) ? v.recipient_count : null;
    const max = options.maxRecipients ?? DEFAULT_MAX_RECIPIENTS;
    let recipients: RecipientsView;
    if (Array.isArray(v.recipients) && v.recipients.every(r => typeof r === 'string')) {
        const kinds =
            Array.isArray(v.recipient_kinds) && v.recipient_kinds.length === v.recipients.length && v.recipient_kinds.every(isRecipientKind)
                ? (v.recipient_kinds as RecipientKind[])
                : (v.recipients as string[]).map((): RecipientKind => 'to');
        recipients = { kind: 'list', shown: (v.recipients as string[]).slice(0, max), kinds: kinds.slice(0, max), total };
    } else {
        recipients = { kind: 'not-recorded', total };
    }

    const senderDomain = typeof v.sender_domain === 'string' && v.sender_domain ? v.sender_domain.toLowerCase() : null;

    const s = v.sender && typeof v.sender === 'object' ? (v.sender as Record<string, unknown>) : null;
    const sender = s && typeof s.address === 'string' && s.address ? { address: s.address, name: typeof s.name === 'string' && s.name ? s.name : null } : null;

    return {
        id: v.id,
        state: v.state,
        status: v.status,
        enhancedProtection: v.enhanced_protection,
        acceptedAt: v.accepted_at,
        deliveryCount: v.delivery_count,
        acceptedCount: v.accepted_count,
        failedCount: v.failed_count,
        uncertainCount: v.uncertain_count,
        subject,
        recipients,
        senderDomain,
        sender
    };
}

/** The hold of a message submitted on hold. `resolved` is null while it is held. */
export interface HoldView {
    expiresAt: string;
    resolved: { resolution: 'released' | 'cancelled' | 'expired'; time: string } | null;
}

/**
 * Validates the optional `hold` object of a message detail. Absent means the message was not
 * submitted on hold (null); a malformed value is reported as invalid (undefined).
 */
export function readMessageHold(raw: unknown): HoldView | null | undefined {
    if (raw === undefined || raw === null) return null;
    if (typeof raw !== 'object') return undefined;
    const v = raw as Record<string, unknown>;
    if (typeof v.expires_at !== 'string') return undefined;
    const resolution = v.resolution === undefined ? null : v.resolution;
    if (resolution !== null && resolution !== 'released' && resolution !== 'cancelled' && resolution !== 'expired') return undefined;
    const resolvedAt = typeof v.resolved_at === 'string' ? v.resolved_at : null;
    if (resolution === null && resolvedAt === null) return { expiresAt: v.expires_at, resolved: null };
    if (resolution === null || resolvedAt === null) return undefined;
    return { expiresAt: v.expires_at, resolved: { resolution, time: resolvedAt } };
}

/** Whether a delivery is for a To, Cc or Bcc recipient; null when not recorded. */
export function readDeliveryRecipientKind(delivery: unknown): RecipientKind | null {
    if (!delivery || typeof delivery !== 'object') return null;
    const kind = (delivery as Record<string, unknown>).recipient_kind;
    return isRecipientKind(kind) ? kind : null;
}

/** The recorded envelope address of a delivery, or null when not recorded. */
export function readDeliveryRecipient(delivery: unknown): string | null {
    if (!delivery || typeof delivery !== 'object') return null;
    const recipient = (delivery as Record<string, unknown>).recipient;
    return typeof recipient === 'string' && recipient ? recipient : null;
}
