import { InvalidResponseError } from './api-errors';
import type { SubmissionRate } from './openapi-client-generated';

/** Limits from the operator API's SubmissionRate schema. */
export const MAX_PER_MINUTE = 60_000;
export const MAX_BURST = 10_000;
/** Largest Resend batch the submission API accepts; a batch is charged for all of its messages at once. */
export const MAX_RESEND_BATCH = 100;

const numberFormat = new Intl.NumberFormat('en-US');

export function formatCount(value: number): string {
    return numberFormat.format(value);
}

export function isDisabled(rate: SubmissionRate): boolean {
    return rate.per_minute === 0 && rate.burst === 0;
}

function isWhole(value: unknown, max: number): value is number {
    return typeof value === 'number' && Number.isInteger(value) && value >= 0 && value <= max;
}

/** Validates a response body against the contract: integers in range, both zero or both positive. */
export function parseSubmissionRate(data: unknown): SubmissionRate {
    const candidate = data as Partial<SubmissionRate> | null | undefined;
    const perMinute = candidate?.per_minute;
    const burst = candidate?.burst;
    if (!isWhole(perMinute, MAX_PER_MINUTE) || !isWhole(burst, MAX_BURST) || (perMinute === 0) !== (burst === 0)) {
        throw new InvalidResponseError();
    }
    return { per_minute: perMinute, burst };
}

export type FieldResult = { value: number; error: null } | { value: null; error: string };

function validateField(raw: string | number, label: string, max: number): FieldResult {
    const text = String(raw ?? '').trim();
    if (!text) return { value: null, error: `Enter ${label}.` };
    if (!/^\d+$/.test(text)) return { value: null, error: 'Enter a whole number, without separators or decimals.' };
    const value = Number(text);
    if (value < 1) return { value: null, error: 'Enter at least 1. To reject all new messages, choose “Block new messages” instead.' };
    if (value > max) return { value: null, error: `Enter at most ${formatCount(max)}.` };
    return { value, error: null };
}

/** Mirrors the server: 1-60,000 messages per minute. The server remains authoritative. */
export function validatePerMinute(raw: string | number): FieldResult {
    return validateField(raw, 'a number of messages per minute', MAX_PER_MINUTE);
}

/** Mirrors the server: 1-10,000 messages. The server remains authoritative. */
export function validateBurst(raw: string | number): FieldResult {
    return validateField(raw, 'a burst size', MAX_BURST);
}

/** A duration in seconds as plain text, rounded to a useful precision. */
export function formatDuration(seconds: number): string {
    const plural = (n: number, unit: string) => `${formatCount(n)} ${unit}${n === 1 ? '' : 's'}`;
    if (seconds < 1) return 'under a second';
    if (seconds < 120) return plural(Math.round(seconds), 'second');
    const minutes = seconds / 60;
    if (minutes < 120) return plural(Math.round(minutes), 'minute');
    const hours = minutes / 60;
    if (hours < 48) return plural(Math.round(hours), 'hour');
    return plural(Math.round(hours / 24), 'day');
}

/** Sustained rate as text, for example “600 messages per minute (10 per second)”. */
export function describeSustained(perMinute: number): string {
    const base = `${formatCount(perMinute)} message${perMinute === 1 ? '' : 's'} per minute`;
    if (perMinute >= 60) {
        const perSecond = Math.round((perMinute / 60) * 10) / 10;
        return `${base} (${formatCount(perSecond)} per second)`;
    }
    return `${base} (one every ${formatDuration(60 / perMinute)})`;
}

/** How long an empty bucket takes to refill to the full burst. */
export function describeRefill(rate: SubmissionRate): string {
    return formatDuration((rate.burst / rate.per_minute) * 60);
}
