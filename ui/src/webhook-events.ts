import type { WebhookEventType, WebhookFormat } from '@/openapi-client-generated';

export interface WebhookEventGroup {
    label: string;
    events: readonly WebhookEventType[];
}

export interface WebhookFormatInfo {
    label: string;
    /** Short tag for the endpoint table. */
    tag: string;
    /** What the signing secret is called, or null when the format has none. */
    secretKind: string | null;
    groups: readonly WebhookEventGroup[];
    /** Delivers received mail instead of events about sent messages; it has no event filter. */
    receivesMail?: boolean;
}

/**
 * Formats in the order offered, each with its own event vocabulary. Mirrors the
 * operator API's WebhookFormat and the server's per-format event types.
 */
export const WEBHOOK_FORMATS: Readonly<Record<WebhookFormat, WebhookFormatInfo>> = {
    thinpost: {
        label: 'Thinpost',
        tag: 'Thinpost',
        secretKind: 'signing secret',
        groups: [
            {
                label: 'Delivery',
                events: ['accepted', 'rejected', 'route_rejected', 'uncertain', 'null_mx', 'domain_not_found', 'retry_expired', 'suppressed']
            },
            {
                label: 'Held or failed before sending',
                events: ['recipient_policy_denied', 'tracking_unavailable', 'tracking_abandoned', 'preparation_failed']
            },
            { label: 'Bounces', events: ['bounce_received_unverified'] },
            { label: 'Tracking', events: ['opened', 'clicked'] },
            { label: 'Recipient feedback', events: ['unsubscribed', 'complained'] }
        ]
    },
    postmark: {
        label: 'Postmark',
        tag: 'Postmark',
        secretKind: null,
        groups: [{ label: 'Events', events: ['Delivery', 'Bounce', 'Open', 'SubscriptionChange', 'SpamComplaint'] }]
    },
    resend: {
        label: 'Resend',
        tag: 'Resend',
        secretKind: 'webhook signing secret',
        groups: [
            {
                label: 'Events',
                events: ['email.delivered', 'email.delivery_delayed', 'email.bounced', 'email.failed', 'email.opened', 'email.suppressed', 'email.complained']
            }
        ]
    },
    mailgun: {
        label: 'Mailgun',
        tag: 'Mailgun',
        secretKind: 'webhook signing key',
        groups: [
            {
                label: 'Events',
                events: ['delivered', 'temporary_fail', 'permanent_fail', 'opened', 'unsubscribed', 'complained']
            }
        ]
    },
    thinpost_inbound: {
        label: 'Inbound mail (Thinpost)',
        tag: 'Inbound mail',
        secretKind: 'signing secret',
        receivesMail: true,
        groups: [{ label: 'Events', events: ['inbound_received'] }]
    }
};

export const WEBHOOK_FORMAT_ORDER: readonly WebhookFormat[] = ['thinpost', 'postmark', 'resend', 'mailgun', 'thinpost_inbound'];

export function eventsOfFormat(format: WebhookFormat): WebhookEventType[] {
    return WEBHOOK_FORMATS[format].groups.flatMap(group => [...group.events]);
}
