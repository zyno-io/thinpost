import { InvalidResponseError } from './api-errors';
import { DefaultApi, type PendingQuota, type RecipientQuota } from './openapi-client-generated';
import { type FieldResult, formatCount } from './submission-rate';

/** Limits from the operator API's RecipientQuota and PendingQuota schemas. */
export const MAX_RECIPIENT_LIMIT = 1_000_000;
export const MAX_PENDING_LIMIT = 100_000;

export type CountLimitKind = 'recipient' | 'pending';

/** One limit as the console shows it, whichever endpoint it came from. */
export interface CountLimit {
    limit: number;
    /** Recipients counted today (UTC), or messages pending now. */
    usage: number;
    /** Pending only: more messages are pending than were counted, so `usage` is a lower bound. */
    truncated: boolean;
    /** Recipient only: the UTC day the count belongs to. */
    day: string | null;
}

export interface CountLimitConfig {
    kind: CountLimitKind;
    max: number;
    /** Panel heading and the topic of its help button. */
    title: string;
    /** Lower-case name used in sentences and toasts. */
    noun: string;
    /** What the limit counts, as in “recipients per UTC day”. */
    unit: string;
    /** Label of the modal's number field. */
    fieldLabel: string;
    /** Label of the usage row. */
    usageLabel: string;
    load(appId: string): Promise<unknown>;
    save(appId: string, limit: number): Promise<unknown>;
    parse(data: unknown): CountLimit;
}

function isWhole(value: unknown, max = Number.MAX_SAFE_INTEGER): value is number {
    return typeof value === 'number' && Number.isInteger(value) && value >= 0 && value <= max;
}

/** Validates a recipient-quota response against the contract. */
export function parseRecipientQuota(data: unknown): CountLimit {
    const candidate = data as Partial<RecipientQuota> | null | undefined;
    if (!isWhole(candidate?.daily_limit, MAX_RECIPIENT_LIMIT) || !isWhole(candidate?.used) || typeof candidate?.day !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(candidate.day)) {
        throw new InvalidResponseError();
    }
    return { limit: candidate.daily_limit, usage: candidate.used, truncated: false, day: candidate.day };
}

/** Validates a pending-quota response against the contract. */
export function parsePendingQuota(data: unknown): CountLimit {
    const candidate = data as Partial<PendingQuota> | null | undefined;
    if (!isWhole(candidate?.limit, MAX_PENDING_LIMIT) || !isWhole(candidate?.pending, MAX_PENDING_LIMIT) || typeof candidate?.truncated !== 'boolean') {
        throw new InvalidResponseError();
    }
    return { limit: candidate.limit, usage: candidate.pending, truncated: candidate.truncated, day: null };
}

export const COUNT_LIMITS: Record<CountLimitKind, CountLimitConfig> = {
    recipient: {
        kind: 'recipient',
        max: MAX_RECIPIENT_LIMIT,
        title: 'Daily recipient limit',
        noun: 'daily recipient limit',
        unit: 'recipients per UTC day',
        fieldLabel: 'Recipients per UTC day',
        usageLabel: 'Used today',
        load: appId => DefaultApi.getRecipientQuota({ path: { appId } }).then(r => r.data),
        save: (appId, daily_limit) => DefaultApi.setRecipientQuota({ path: { appId }, body: { daily_limit } }).then(r => r.data),
        parse: parseRecipientQuota
    },
    pending: {
        kind: 'pending',
        max: MAX_PENDING_LIMIT,
        title: 'Pending-message limit',
        noun: 'pending-message limit',
        unit: 'pending messages',
        fieldLabel: 'Pending messages',
        usageLabel: 'Pending now',
        load: appId => DefaultApi.getPendingQuota({ path: { appId } }).then(r => r.data),
        save: (appId, limit) => DefaultApi.setPendingQuota({ path: { appId }, body: { limit } }).then(r => r.data),
        parse: parsePendingQuota
    }
};

export function isBlocked(value: CountLimit): boolean {
    return value.limit === 0;
}

/** Mirrors the server: 1 to the maximum. Zero is chosen with “Block new messages”. The server remains authoritative. */
export function validateCountLimit(raw: string | number, max: number): FieldResult {
    const text = String(raw ?? '').trim();
    if (!text) return { value: null, error: 'Enter a limit.' };
    if (!/^\d+$/.test(text)) return { value: null, error: 'Enter a whole number, without separators or decimals.' };
    const value = Number(text);
    if (value < 1) return { value: null, error: 'Enter at least 1. To reject all new messages, choose “Block new messages” instead.' };
    if (value > max) return { value: null, error: `Enter at most ${formatCount(max)}.` };
    return { value, error: null };
}

/** Usage against the limit, for example “120 of 1,000 recipients today (UTC)”. */
export function describeUsage(config: CountLimitConfig, value: CountLimit): string {
    const count = `${formatCount(value.usage)}${value.truncated ? ' or more' : ''}`;
    return config.kind === 'recipient' ? `${count} of ${formatCount(value.limit)} recipients` : `${count} of ${formatCount(value.limit)} messages`;
}

/** The limit as text for toasts and notes. */
export function summarizeLimit(config: CountLimitConfig, value: CountLimit): string {
    return isBlocked(value) ? 'new messages blocked' : `${formatCount(value.limit)} ${config.unit}`;
}
