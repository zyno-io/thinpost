import { InvalidResponseError } from './api-errors';
import type { DeliveryTracking, RuntimeInfo, TrackingDefaults, TrackingQuota } from './openapi-client-generated';
import { formatCount, type FieldResult } from './submission-rate';

/** Limits from the API's TrackingQuota schema. */
export const MAX_DAILY_LIMIT = 1_000_000;

const isCount = (value: unknown, max = Number.MAX_SAFE_INTEGER): value is number =>
    typeof value === 'number' && Number.isInteger(value) && value >= 0 && value <= max;

/** Validates the runtime report: boolean features, and a tracking block only with a string origin and a 1-1000 cap. */
export function parseRuntimeInfo(data: unknown): RuntimeInfo {
    const candidate = data as Partial<RuntimeInfo> | null | undefined;
    const features = candidate?.features as Partial<RuntimeInfo['features']> | undefined;
    if (
        typeof features?.submission !== 'boolean' ||
        typeof features.tracking !== 'boolean'
    ) {
        throw new InvalidResponseError();
    }
    const info: RuntimeInfo = { features: { submission: features.submission, tracking: features.tracking } };
    const tracking = candidate?.tracking;
    if (tracking !== undefined) {
        if (typeof tracking.origin !== 'string' || !tracking.origin || !isCount(tracking.link_event_limit, 1000) || tracking.link_event_limit < 1) {
            throw new InvalidResponseError();
        }
        info.tracking = { origin: tracking.origin, link_event_limit: tracking.link_event_limit };
    }
    return info;
}

export function parseTrackingQuota(data: unknown): TrackingQuota {
    const candidate = data as Partial<TrackingQuota> | null | undefined;
    if (
        !isCount(candidate?.daily_limit, MAX_DAILY_LIMIT) ||
        !isCount(candidate.used, MAX_DAILY_LIMIT) ||
        !isCount(candidate.suppressed, 1_000_000_000) ||
        typeof candidate.day !== 'string' ||
        !/^\d{4}-\d{2}-\d{2}$/.test(candidate.day)
    ) {
        throw new InvalidResponseError();
    }
    return { daily_limit: candidate.daily_limit, used: candidate.used, suppressed: candidate.suppressed, day: candidate.day };
}

/** Mirrors the server: 0 to 1,000,000 whole events per UTC day. The server remains authoritative. */
export function validateDailyLimit(raw: string | number): FieldResult {
    const text = String(raw ?? '').trim();
    if (!text) return { value: null, error: 'Enter a daily limit, or 0 to record nothing.' };
    if (!/^\d+$/.test(text)) return { value: null, error: 'Enter a whole number, without separators or decimals.' };
    const value = Number(text);
    if (value > MAX_DAILY_LIMIT) return { value: null, error: `Enter at most ${formatCount(MAX_DAILY_LIMIT)}.` };
    return { value, error: null };
}

/** Reads a delivery's optional engagement; a malformed value is treated as not reported. */
export function readDeliveryTracking(delivery: { tracking?: unknown }): DeliveryTracking | null {
    const t = delivery.tracking as Partial<DeliveryTracking> | null | undefined;
    if (!t || !isCount(t.opens) || !isCount(t.clicks)) return null;
    const out: DeliveryTracking = { opens: t.opens, clicks: t.clicks };
    if (typeof t.last_event_at === 'string' && t.last_event_at.length <= 64) out.last_event_at = t.last_event_at;
    return out;
}

/** Validates the tracking-defaults response: two booleans. */
export function parseTrackingDefaults(data: unknown): TrackingDefaults {
    const candidate = data as Partial<TrackingDefaults> | null | undefined;
    if (typeof candidate?.opens !== 'boolean' || typeof candidate.clicks !== 'boolean') throw new InvalidResponseError();
    return { opens: candidate.opens, clicks: candidate.clicks };
}
