/**
 * Reads the optional per-delivery attempt fields of the message detail
 * response (retry_attempts, next_attempt_at, last_completed_outcome,
 * last_smtp_code). Field names come from the generated DeliverySummary type,
 * where all four are optional. Control planes deployed before these fields
 * existed omit them, and the generated types are not checked at runtime, so
 * every value is still treated as untrusted and checked before it is shown. A
 * value that fails its check is treated as not reported rather than displayed.
 */
import type { DeliverySummary } from '@/openapi-client-generated';
import type { StateLabel } from '@/states';

type ProgressField = 'retry_attempts' | 'next_attempt_at' | 'last_completed_outcome' | 'last_smtp_code';

/** The contract's optional progress fields, with each value unverified until checked. */
type UntrustedProgress = { [K in keyof Pick<DeliverySummary, ProgressField>]?: unknown };

export interface DeliveryProgress {
    /** Attempts counted by the retry policy; null when not reported. */
    retryAttempts: number | null;
    /** Earliest eligibility for a queued delivery; null when absent or invalid. */
    nextAttemptAt: string | null;
    /** True when the response describes a completed attempt (either field is present). */
    hasCompletedAttempt: boolean;
    /** Outcome of the latest completed attempt; null when absent or invalid. */
    lastOutcome: StateLabel | null;
    /** Identifier of the outcome when this console recognizes it; never an unrecognized value. */
    lastOutcomeKey: string | null;
    /** SMTP code of that attempt (0 means none recorded); null when absent or invalid. */
    lastSmtpCode: number | null;
}

const FIELDS: readonly ProgressField[] = ['retry_attempts', 'next_attempt_at', 'last_completed_outcome', 'last_smtp_code'];

const MAX_TIME_LENGTH = 64;

const LOCAL = 'Thinpost reached this conclusion itself; it is not a reply from the recipient\'s mail server.';

const OUTCOMES: Record<string, { label: string; description: string }> = {
    accepted: {
        label: 'Accepted by mail server',
        description: 'The recipient\'s mail server accepted the message. That doesn\'t guarantee it reached the inbox.'
    },
    rejected: {
        label: 'Rejected',
        description:
            'The attempt did not succeed. With an SMTP code, the recipient\'s mail server rejected it (4xx means try again later, 5xx means permanently refused). Without one, no reply was recorded.'
    },
    route_rejected: {
        label: 'Rejected by one mail server',
        description: 'One of the recipient domain’s mail servers rejected the attempt. A later attempt may use another one.'
    },
    uncertain: { label: 'Outcome unknown', description: 'Thinpost can\'t tell whether the recipient\'s mail server accepted the message.' },
    null_mx: { label: 'Domain does not accept mail', description: `The recipient domain says it doesn't accept email (a null MX record). ${LOCAL}` },
    domain_not_found: { label: 'Domain not found', description: `The recipient domain doesn't exist. ${LOCAL}` },
    recipient_policy_denied: {
        label: 'Blocked by recipient rules',
        description: `The recipient rules set by your Thinpost administrator don't allow this recipient, so no attempt was made. These aren't counted as attempts. ${LOCAL}`
    },
    tracking_unavailable: {
        label: 'Tracking unavailable',
        description: `The delivery was held because its tracking links were unavailable, so no attempt was made. ${LOCAL}`
    },
    suppressed: { label: 'Suppressed', description: `The recipient was on the app's suppression list, so no attempt was made. ${LOCAL}` },
    retry_expired: { label: 'Retry period expired', description: `Thinpost stopped retrying because too much time passed. ${LOCAL}` }
};

function isInteger(value: unknown, min: number, max: number): value is number {
    return typeof value === 'number' && Number.isSafeInteger(value) && value >= min && value <= max;
}

function readTime(value: unknown): string | null {
    if (typeof value !== 'string' || !value || value.length > MAX_TIME_LENGTH) return null;
    return Number.isNaN(new Date(value).getTime()) ? null : value;
}

const UNRECOGNIZED_OUTCOME: StateLabel = {
    label: 'Unrecognized outcome',
    tone: 'inactive',
    description: 'This console doesn\'t recognize the reported outcome, so it isn\'t shown.'
};

/** Only outcomes this console knows are labeled; any other value is replaced by a generic label and never echoed. */
export function attemptOutcome(value: unknown): StateLabel {
    const known = typeof value === 'string' && Object.hasOwn(OUTCOMES, value) ? OUTCOMES[value] : undefined;
    return known ? { label: known.label, tone: 'inactive', description: known.description } : { ...UNRECOGNIZED_OUTCOME };
}

function untrusted(delivery: unknown): UntrustedProgress {
    return delivery && typeof delivery === 'object' ? (delivery as UntrustedProgress) : {};
}

/** Whether a delivery carries any of the attempt fields; older control planes send none. */
export function hasDeliveryProgress(delivery: unknown): boolean {
    const v = untrusted(delivery);
    return FIELDS.some(field => v[field] !== undefined && v[field] !== null);
}

export function readDeliveryProgress(delivery: unknown): DeliveryProgress {
    const v = untrusted(delivery);
    const outcome = v.last_completed_outcome;
    const code = v.last_smtp_code;
    const hasOutcome = outcome !== undefined && outcome !== null;
    const hasCode = code !== undefined && code !== null;
    return {
        retryAttempts: isInteger(v.retry_attempts, 0, Number.MAX_SAFE_INTEGER) ? v.retry_attempts : null,
        nextAttemptAt: readTime(v.next_attempt_at),
        hasCompletedAttempt: hasOutcome || hasCode,
        lastOutcome: hasOutcome ? attemptOutcome(outcome) : null,
        lastOutcomeKey: typeof outcome === 'string' && Object.hasOwn(OUTCOMES, outcome) ? outcome : null,
        lastSmtpCode: isInteger(code, 0, 599) ? code : null
    };
}

export interface SmtpCodeLabel {
    text: string;
    description: string;
}

export function smtpCodeLabel(code: number): SmtpCodeLabel {
    if (code === 0) {
        return {
            text: 'No reply recorded',
            description: 'No SMTP code was recorded for this attempt, for example because it ended before the mail server replied.'
        };
    }
    if (code >= 200 && code <= 299) {
        return {
            text: `SMTP ${code} (accepted)`,
            description: 'The recipient\'s mail server accepted the message. That doesn\'t guarantee it reached the inbox.'
        };
    }
    if (code >= 400 && code <= 499) return { text: `SMTP ${code} (temporary)`, description: 'The recipient\'s mail server asked Thinpost to try again later (a 4xx reply).' };
    if (code >= 500 && code <= 599) return { text: `SMTP ${code} (permanent)`, description: 'The mail server replied with a permanent-failure code (5xx).' };
    return { text: `SMTP ${code}`, description: 'The SMTP code the recipient\'s mail server replied with for this attempt.' };
}
