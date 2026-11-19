import { InvalidResponseError } from './api-errors';
import type { AttentionPage, Operations, RetryPolicy } from './openapi-client-generated';
import { formatCount } from './submission-rate';

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** Server limits for the retry policy (RetryPolicy schema). */
export const POLICY_LIMITS = {
    attempts: { min: 1, max: 1000 },
    initial: { min: MINUTE, max: DAY },
    maxDelay: { min: MINUTE, max: DAY },
    maxAge: { min: HOUR, max: 30 * DAY }
} as const;

/** A deploy closure older than this is probably stuck. */
export const STALE_DEPLOY_CLOSURE_MS = 30 * MINUTE;

const isCount = (value: unknown): value is number => typeof value === 'number' && Number.isInteger(value) && value >= 0;
const isTime = (value: unknown): value is string => typeof value === 'string' && value.length <= 64 && !Number.isNaN(new Date(value).getTime());
const isOptionalTime = (value: unknown): boolean => value === undefined || isTime(value);
const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value);

function countsOk(value: unknown, keys: readonly string[]): value is Record<string, unknown> {
    return isRecord(value) && keys.every(key => isCount(value[key])) && isOptionalTime(value.oldest_due_at);
}

export function parseRetryPolicy(value: unknown): RetryPolicy {
    if (
        !isRecord(value) ||
        !isCount(value.max_attempts) ||
        !isCount(value.initial_delay_ms) ||
        !isCount(value.max_delay_ms) ||
        !isCount(value.max_age_ms) ||
        !isCount(value.revision) ||
        value.revision < 1 ||
        !isTime(value.changed_at) ||
        (value.changed_by !== undefined && typeof value.changed_by !== 'string')
    ) {
        throw new InvalidResponseError();
    }
    return value as unknown as RetryPolicy;
}

/** Checks the snapshot's shape so a stale or foreign response never renders as zeros. */
export function parseOperations(data: unknown): Operations {
    if (!isRecord(data) || !isTime(data.generated_at) || !isRecord(data.gate) || !Array.isArray(data.workers)) throw new InvalidResponseError();
    const gate = data.gate;
    if (
        !['open', 'closed', 'recovery'].includes(gate.mode as string) ||
        !isCount(gate.revision) ||
        !isTime(gate.changed_at) ||
        typeof gate.generation_matches !== 'boolean' ||
        typeof gate.quarantine_complete !== 'boolean' ||
        (gate.changed_by !== undefined && typeof gate.changed_by !== 'string')
    ) {
        throw new InvalidResponseError();
    }
    for (const worker of data.workers) {
        if (!isRecord(worker) || typeof worker.worker_id !== 'string' || typeof worker.controller_id !== 'string' || !isCount(worker.epoch) || !isTime(worker.expires_at) || typeof worker.live !== 'boolean') {
            throw new InvalidResponseError();
        }
    }
    if (
        !countsOk(data.messages, ['queued_due', 'queued_waiting', 'preparing', 'preparation_failed', 'recovery_hold']) ||
        // Servers before message holds omit on_hold; the console may poll one during a rolling deploy.
        (data.messages.on_hold !== undefined && !isCount(data.messages.on_hold)) ||
        !countsOk(data.deliveries, ['queued_due', 'queued_waiting', 'in_flight', 'uncertain', 'recipient_policy_held', 'tracking_held', 'recovery_hold']) ||
        !countsOk(data.webhooks, ['queued_due', 'queued_waiting', 'in_flight', 'recovery_hold'])
    ) {
        throw new InvalidResponseError();
    }
    parseInboundToday(data.inbound);
    parseRetryPolicy(data.retry_policy);
    return data as unknown as Operations;
}

export const INBOUND_COUNT_KEYS = ['accepted', 'correlated', 'duplicate', 'tempfail_routing', 'tempfail_limit', 'tempfail_internal'] as const;

/**
 * Today's inbound counters. The field is absent from older Thinpost servers, which is not an error: the card then says
 * the counts aren't available. Anything else that does not match the contract is rejected.
 */
export function parseInboundToday(value: unknown): void {
    if (value === undefined) return;
    if (!isRecord(value) || typeof value.day !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(value.day) || !INBOUND_COUNT_KEYS.every(key => isCount(value[key]))) {
        throw new InvalidResponseError();
    }
}

export const ATTENTION_KINDS = ['uncertain', 'recipient_policy_held', 'tracking_held', 'preparation_failed', 'recovery_hold'] as const;

export function parseAttentionPage(data: unknown): AttentionPage {
    if (!isRecord(data) || !Array.isArray(data.items) || (data.next_cursor !== undefined && typeof data.next_cursor !== 'string')) throw new InvalidResponseError();
    for (const item of data.items) {
        if (
            !isRecord(item) ||
            typeof item.app_id !== 'string' ||
            typeof item.app_name !== 'string' ||
            typeof item.message_id !== 'string' ||
            (item.delivery_id !== undefined && typeof item.delivery_id !== 'string') ||
            !ATTENTION_KINDS.includes(item.kind as (typeof ATTENTION_KINDS)[number]) ||
            !isTime(item.since)
        ) {
            throw new InvalidResponseError();
        }
    }
    return data as unknown as AttentionPage;
}

export const ATTENTION_LABELS: Record<(typeof ATTENTION_KINDS)[number], { label: string; tone: 'warning' | 'danger' | '' }> = {
    uncertain: { label: 'Outcome unknown', tone: 'danger' },
    recipient_policy_held: { label: 'Held: recipient rules', tone: 'warning' },
    tracking_held: { label: 'Held: tracking', tone: 'warning' },
    preparation_failed: { label: 'Preparation failed', tone: 'danger' },
    recovery_hold: { label: 'Held after recovery', tone: 'warning' }
};

/** Compact duration for a policy value: `30 min`, `3 h`, `1 h 30 min`, `5 d`. */
export function formatMs(ms: number): string {
    if (ms < MINUTE) return `${formatCount(Math.round(ms / 1000))} s`;
    const days = Math.floor(ms / DAY);
    const hours = Math.floor((ms % DAY) / HOUR);
    const minutes = Math.round((ms % HOUR) / MINUTE);
    const parts = [days && `${days} d`, hours && `${hours} h`, minutes && `${minutes} min`].filter(Boolean);
    return parts.join(' ') || '0 min';
}

/** Age of a past time for “oldest due”: `4 min`, `2 h 5 min`; never negative. */
export function formatAge(from: string, now: number): string {
    const ms = Math.max(0, now - new Date(from).getTime());
    return ms < MINUTE ? '<1 min' : formatMs(Math.floor(ms / MINUTE) * MINUTE);
}

/** Time left until a lease expires; null once it has passed. */
export function formatRemaining(until: string, now: number): string | null {
    const ms = new Date(until).getTime() - now;
    if (ms <= 0) return null;
    return ms < MINUTE ? `${Math.ceil(ms / 1000)} s` : formatMs(Math.floor(ms / MINUTE) * MINUTE);
}

/** True for a deploy-owned closure that has outlasted STALE_DEPLOY_CLOSURE_MS. */
export function isStaleDeployClosure(gate: Operations['gate'], now: number): boolean {
    return gate.mode === 'closed' && !!gate.changed_by?.startsWith('deploy:') && now - new Date(gate.changed_at).getTime() > STALE_DEPLOY_CLOSURE_MS;
}

export interface PolicyDraft {
    maxAttempts: string;
    initialMinutes: string;
    maxDelayMinutes: string;
    maxAgeMinutes: string;
}

export interface PolicyFields {
    max_attempts: number;
    initial_delay_ms: number;
    max_delay_ms: number;
    max_age_ms: number;
}

const minutesText = (ms: number) => String(Math.round((ms / MINUTE) * 1000) / 1000);

export function draftFromPolicy(policy: RetryPolicy): PolicyDraft {
    return {
        maxAttempts: String(policy.max_attempts),
        initialMinutes: minutesText(policy.initial_delay_ms),
        maxDelayMinutes: minutesText(policy.max_delay_ms),
        maxAgeMinutes: minutesText(policy.max_age_ms)
    };
}

type Parsed = { ms: number; error: null } | { ms: null; error: string };

function parseMinutes(raw: string, label: string, limit: { min: number; max: number }): Parsed {
    const text = raw.trim();
    if (!text) return { ms: null, error: `Enter ${label}.` };
    if (!/^\d+(\.\d{1,3})?$/.test(text)) return { ms: null, error: 'Enter minutes as a number, for example 30 or 1.5.' };
    const ms = Math.round(Number(text) * MINUTE);
    if (ms < limit.min) return { ms: null, error: `Enter at least ${formatMs(limit.min)}.` };
    if (ms > limit.max) return { ms: null, error: `Enter at most ${formatMs(limit.max)}.` };
    return { ms, error: null };
}

export interface PolicyValidation {
    value: PolicyFields | null;
    errors: { maxAttempts?: string; initial?: string; maxDelay?: string; maxAge?: string };
}

/** Mirrors the server's rules; the server stays authoritative. */
export function validatePolicyDraft(draft: PolicyDraft): PolicyValidation {
    const errors: PolicyValidation['errors'] = {};
    const attemptsText = draft.maxAttempts.trim();
    const attempts = /^\d+$/.test(attemptsText) ? Number(attemptsText) : NaN;
    if (!attemptsText) errors.maxAttempts = 'Enter the maximum number of attempts.';
    else if (!Number.isInteger(attempts)) errors.maxAttempts = 'Enter a whole number.';
    else if (attempts < POLICY_LIMITS.attempts.min || attempts > POLICY_LIMITS.attempts.max) {
        errors.maxAttempts = `Enter ${POLICY_LIMITS.attempts.min} to ${formatCount(POLICY_LIMITS.attempts.max)}.`;
    }
    const initial = parseMinutes(draft.initialMinutes, 'the initial delay', POLICY_LIMITS.initial);
    const maxDelay = parseMinutes(draft.maxDelayMinutes, 'the maximum delay', POLICY_LIMITS.maxDelay);
    const maxAge = parseMinutes(draft.maxAgeMinutes, 'the maximum age', POLICY_LIMITS.maxAge);
    if (initial.error) errors.initial = initial.error;
    if (maxDelay.error) errors.maxDelay = maxDelay.error;
    if (maxAge.error) errors.maxAge = maxAge.error;
    if (initial.ms !== null && maxDelay.ms !== null && maxDelay.ms < initial.ms) errors.maxDelay = 'Must be at least the initial delay.';
    if (initial.ms !== null && maxAge.ms !== null && maxAge.ms < initial.ms) errors.maxAge = 'Must be at least the initial delay.';
    if (Object.keys(errors).length) return { value: null, errors };
    return { value: { max_attempts: attempts, initial_delay_ms: initial.ms!, max_delay_ms: maxDelay.ms!, max_age_ms: maxAge.ms! }, errors };
}

export function samePolicy(fields: PolicyFields, policy: RetryPolicy): boolean {
    return (
        fields.max_attempts === policy.max_attempts &&
        fields.initial_delay_ms === policy.initial_delay_ms &&
        fields.max_delay_ms === policy.max_delay_ms &&
        fields.max_age_ms === policy.max_age_ms
    );
}
