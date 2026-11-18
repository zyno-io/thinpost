/**
 * Labels for the state strings the API returns. The contract types
 * states as free-form strings, so an unrecognized value is shown verbatim
 * rather than mapped to a guess.
 */
export interface StateLabel {
    label: string;
    /** Badge styling only; it does not imply anything beyond the label. */
    tone: 'active' | 'inactive' | 'warning' | 'danger';
    description?: string;
}

const MESSAGE_STATES: Record<string, StateLabel> = {
    queued: { label: 'Queued', tone: 'inactive', description: 'Submitted by your app and waiting to be prepared for sending.' },
    preparing: { label: 'Preparing', tone: 'inactive', description: 'Being prepared for each recipient.' },
    prepared: { label: 'Prepared', tone: 'inactive', description: 'Ready to send to each recipient. This doesn\'t mean it has been delivered.' }
};

const MESSAGE_STATUSES: Record<string, StateLabel> = {
    queued: { label: 'Queued', tone: 'inactive', description: 'Submitted; not yet prepared for sending.' },
    preparing: { label: 'Preparing', tone: 'inactive', description: 'Being prepared for each recipient.' },
    sending: { label: 'Sending', tone: 'inactive', description: 'At least one recipient is still waiting to be sent.' },
    retrying: { label: 'Retrying', tone: 'warning', description: 'A recipient\'s mail server didn\'t accept it yet. Thinpost is trying again.' },
    delivered: { label: 'Delivered', tone: 'active', description: "Accepted by every recipient's mail server." },
    partially_delivered: { label: 'Partially delivered', tone: 'warning', description: 'Some recipients\' mail servers accepted it; others did not.' },
    failed: { label: 'Failed', tone: 'danger', description: 'No recipient\'s mail server accepted it.' },
    suppressed: { label: 'Suppressed', tone: 'inactive', description: 'Every recipient was on the suppression list, so nothing was sent.' },
    held: { label: 'Held', tone: 'warning', description: 'Held back after a data recovery and not sent.' },
    attention: { label: 'Needs attention', tone: 'warning', description: 'A delivery is on hold or its result is unknown. Open the message for details.' },
    unknown: { label: 'Unknown', tone: 'inactive', description: 'The status could not be determined.' }
};

const DELIVERY_STATES: Record<string, StateLabel> = {
    queued: { label: 'Queued', tone: 'inactive', description: 'Waiting to be sent, possibly after a retry delay.' },
    in_flight: { label: 'Sending', tone: 'inactive', description: 'Being sent now; no result has been recorded yet.' },
    accepted: {
        label: 'Accepted',
        tone: 'active',
        description: 'The recipient\'s mail server accepted it. That doesn\'t guarantee it reached the inbox.'
    },
    failed: {
        label: 'Failed',
        tone: 'danger',
        description:
            'It won\'t be tried again. Usually the recipient\'s mail server rejected it permanently. A delivery held because tracking is unavailable can also be marked failed without another attempt.'
    },
    uncertain: {
        label: 'Uncertain',
        tone: 'warning',
        description: 'Thinpost can\'t tell whether the recipient\'s mail server accepted it. It is on hold and isn\'t retried automatically.'
    },
    recipient_policy_held: {
        label: 'Held by recipient rules',
        tone: 'warning',
        description:
            'Held because the recipient rules set by your Thinpost administrator don\'t allow this recipient. It isn\'t retried automatically and isn\'t counted as failed. Retrying checks the rules again but can\'t override them.'
    },
    tracking_held: {
        label: 'Held: tracking unavailable',
        tone: 'warning',
        description:
            'Held because its tracking links were revoked, retired or about to expire. It isn\'t sent or retried automatically and isn\'t counted as failed. The only option is to mark it failed.'
    }
};

const WEBHOOK_DELIVERY_STATES: Record<string, StateLabel> = {
    queued: {
        label: 'Queued',
        tone: 'inactive',
        description: 'Waiting to be sent, possibly after a retry delay.'
    },
    in_flight: { label: 'Sending', tone: 'inactive', description: 'An attempt is under way or its result hasn\'t been recorded yet.' },
    delivered: { label: 'Delivered', tone: 'active', description: 'Your receiver accepted the event (a 2xx response).' },
    cancelled: { label: 'Cancelled', tone: 'inactive', description: 'The webhook was deleted before this event was sent. It can\'t be sent again.' },
    recovery_hold: { label: 'On hold', tone: 'inactive', description: 'Held back after a data recovery and not sent. There\'s no way to release it yet.' },
    failed: {
        label: 'Failed',
        tone: 'danger',
        description: 'Automatic retries ran out without your receiver accepting the event. You can send it again manually.'
    }
};

const WEBHOOK_ATTEMPT_STATES: Record<string, StateLabel> = {
    in_flight: { label: 'Sending', tone: 'inactive', description: 'Started; no result has been recorded yet.' },
    succeeded: { label: 'Succeeded', tone: 'active', description: 'Your receiver accepted the event (a 2xx response).' },
    failed: { label: 'Failed', tone: 'danger', description: 'Your receiver returned an error, or the request couldn\'t be completed.' },
    unknown: {
        label: 'Unknown',
        tone: 'warning',
        description: 'No result was recorded. Your receiver may still have received the event.'
    },
    deferred: {
        label: 'Postponed',
        tone: 'inactive',
        description: 'Thinpost was busy, so nothing was sent. The event was put back in the queue and will be tried again shortly.'
    },
    content_unavailable: {
        label: 'Message gone',
        tone: 'danger',
        description: "The stored message is no longer available (it was removed or kept past its retention period), so it can't be sent."
    }
};

function lookup(table: Record<string, StateLabel>, state: string): StateLabel {
    return (Object.hasOwn(table, state) && table[state]) || { label: state, tone: 'inactive' };
}

export function messageState(state: string): StateLabel {
    return lookup(MESSAGE_STATES, state);
}

export function messageStatus(status: string): StateLabel {
    return lookup(MESSAGE_STATUSES, status);
}

export function deliveryState(state: string): StateLabel {
    return lookup(DELIVERY_STATES, state);
}

export function webhookDeliveryState(state: string): StateLabel {
    return lookup(WEBHOOK_DELIVERY_STATES, state);
}

export function webhookAttemptState(state: string): StateLabel {
    return lookup(WEBHOOK_ATTEMPT_STATES, state);
}

/** Tag modifier class (see `.tag` in base.scss) for a state tone. */
export function tagClass(tone: StateLabel['tone']): string {
    switch (tone) {
        case 'active':
            return 'success';
        case 'warning':
            return 'warning';
        case 'danger':
            return 'danger';
        default:
            return '';
    }
}
