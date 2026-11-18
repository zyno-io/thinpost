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
