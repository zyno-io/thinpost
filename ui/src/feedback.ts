import type { StateLabel } from '@/states';

export interface DeliveryFeedbackTag {
    kind: 'unsubscribe' | 'spam_complaint';
    label: string;
    tone: StateLabel['tone'];
    receivedAt: string;
}

const FEEDBACK_LABELS: Record<DeliveryFeedbackTag['kind'], { label: string; tone: StateLabel['tone'] }> = {
    unsubscribe: { label: 'Unsubscribed', tone: 'warning' },
    spam_complaint: { label: 'Complained', tone: 'danger' }
};

/** Recorded recipient feedback of a delivery. Unknown kinds and malformed entries are dropped, never echoed. */
export function readDeliveryFeedback(delivery: { feedback?: unknown }): DeliveryFeedbackTag[] {
    if (!Array.isArray(delivery.feedback)) return [];
    const tags: DeliveryFeedbackTag[] = [];
    for (const item of delivery.feedback as Array<{ kind?: unknown; received_at?: unknown }>) {
        const kind = item?.kind;
        if ((kind !== 'unsubscribe' && kind !== 'spam_complaint') || typeof item.received_at !== 'string' || item.received_at.length > 64) continue;
        tags.push({ kind, ...FEEDBACK_LABELS[kind], receivedAt: item.received_at });
    }
    return tags;
}

export interface UnsubscribeForwardNote {
    state: 'queued' | 'delivered' | 'failed' | 'not_forwarded';
    /** Plain text for the message page, e.g. "Also unsubscribed at the sender's own link: done". */
    text: string;
    /** A longer explanation for a tooltip. Never the link, only its host. */
    title: string;
    tone: StateLabel['tone'];
}

const FORWARD_PREFIX = "Also unsubscribed at the sender's own link";
const HOST_PATTERN = /^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$/i;

const NOT_AVAILABLE: Record<string, { text: string; title: string }> = {
    mailto_only: {
        text: 'not available (mailto link)',
        title: 'The message only had a mailto unsubscribe link. Thinpost does not write to those. The recipient is still unsubscribed here.'
    },
    no_one_click_post: {
        text: 'not available (no one-click support)',
        title: 'The message had its own unsubscribe link but not the header that says it takes a one-click request, so it was not called. The recipient is still unsubscribed here.'
    },
    unusable_link: {
        text: "not available (link can't be used)",
        title: "The message's own unsubscribe link was malformed or is not a plain public https address, so it was not called. The recipient is still unsubscribed here."
    }
};

const FAILURES: Record<string, string> = {
    blocked_destination: "its address isn't a public one",
    resolution_failed: "its name couldn't be looked up",
    connection_failed: "we couldn't connect",
    tls_failed: 'its secure connection failed',
    timeout: 'it took too long to answer',
    http_status: 'it kept answering with an error',
    rejected: 'it refused the request',
    invalid_link: "the link couldn't be used",
    target_changed: "the message's link no longer matched",
    source_unavailable: "the message couldn't be read",
    app_deleted: 'the app was deleted'
};

/**
 * What became of the call to the message's own unsubscribe link, for a delivery
 * whose recipient unsubscribed. Unknown states and malformed values are
 * dropped, never echoed. Only the link's host is ever shown.
 */
export function readUnsubscribeForward(delivery: { unsubscribe_forward?: unknown }): UnsubscribeForwardNote | null {
    const item = delivery.unsubscribe_forward as { state?: unknown; reason?: unknown; host?: unknown } | null | undefined;
    if (!item || typeof item !== 'object') return null;
    const host = typeof item.host === 'string' && HOST_PATTERN.test(item.host) ? item.host : '';
    const at = host ? ` ${host}` : ' the sender';
    switch (item.state) {
        case 'delivered':
            return { state: 'delivered', text: `${FORWARD_PREFIX}: done`, title: `Thinpost told${at} that the recipient unsubscribed.`, tone: 'active' };
        case 'queued':
            return { state: 'queued', text: `${FORWARD_PREFIX}: in progress`, title: `Thinpost is still trying to tell${at}. It retries for about a day.`, tone: 'warning' };
        case 'failed': {
            const why = typeof item.reason === 'string' ? FAILURES[item.reason] : undefined;
            return {
                state: 'failed',
                text: `${FORWARD_PREFIX}: failed`,
                title: `Thinpost couldn't tell${at}${why ? ` (${why})` : ''}. The recipient is still unsubscribed here, so they will not get more mail from this app.`,
                tone: 'danger'
            };
        }
        case 'not_forwarded': {
            const known = typeof item.reason === 'string' ? NOT_AVAILABLE[item.reason] : undefined;
            if (!known) return null;
            return { state: 'not_forwarded', text: `${FORWARD_PREFIX}: ${known.text}`, title: known.title, tone: 'inactive' };
        }
    }
    return null;
}
