/** The RFC 5322 Message-ID the admin API reports per delivery: `<{32 hex}@{lowercase domain}>`. */
const HEADER = /^<([0-9a-f]{32})@[a-z0-9.-]{1,253}>$/;

/** Message states before any delivery exists; the header is assigned at preparation. */
const UNPREPARED = new Set(['queued', 'preparing', 'preparation_failed']);

export type MessageIdHeaderView =
    /** Every copy carries this one header (messages prepared at schema 38 or later). */
    | { kind: 'shared'; value: string }
    /** Each delivery has its own header (messages prepared earlier); see each delivery's headers. */
    | { kind: 'perRecipient' }
    /** Not assigned yet. */
    | { kind: 'pending' }
    /** The sender domain was not recorded, or no valid value was reported. */
    | { kind: 'unrecorded' };

/**
 * Classifies the reported delivery headers of one message. A header whose local
 * part is the message ID is the shared form; any other valid header is a
 * per-delivery one. Malformed values are ignored, never displayed.
 */
export function readMessageIdHeader(messageId: string, state: string, deliveries: ReadonlyArray<{ message_id_header?: unknown }>): MessageIdHeaderView {
    if (UNPREPARED.has(state) && deliveries.length === 0) return { kind: 'pending' };
    let shared: string | null = null;
    let perRecipient = false;
    for (const delivery of deliveries) {
        const value = delivery.message_id_header;
        if (typeof value !== 'string') continue;
        const match = HEADER.exec(value);
        if (!match) continue;
        if (match[1] === messageId) shared ??= value;
        else perRecipient = true;
    }
    if (perRecipient) return { kind: 'perRecipient' };
    return shared ? { kind: 'shared', value: shared } : { kind: 'unrecorded' };
}
