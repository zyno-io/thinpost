import { InvalidResponseError } from './api-errors';
import type { InboundDomain, InboundLimits } from './openapi-client-generated';
import { formatCount, type FieldResult } from './submission-rate';

/** Limits from the operator API's InboundLimits schema. The server remains authoritative. */
export const MAX_INBOUND_DAILY = 1_000_000;
export const MAX_INBOUND_PENDING = 100_000;

export type InboundState = InboundDomain['state'];
export type MxStatus = NonNullable<InboundDomain['mx_status']>;

export const STATE_LABELS: Record<InboundState, string> = {
    pending: 'Not verified',
    active: 'Verified',
    disabled: 'Disabled'
};

export const STATE_TONES: Record<InboundState, 'warning' | 'success' | ''> = {
    pending: 'warning',
    active: 'success',
    disabled: ''
};

export interface MxDescription {
    label: string;
    tone: 'success' | 'warning' | 'danger' | '';
    /** What the result means for the domain's mail. */
    detail: string;
}

/** Plain-language MX results. The check is informational and never stops a domain from being verified. */
export const MX_DESCRIPTIONS: Record<MxStatus, MxDescription> = {
    thinpost: {
        label: 'Points to Thinpost',
        tone: 'success',
        detail: 'Every MX record names the Thinpost mail host, so mail sent to this domain reaches Thinpost.'
    },
    mixed: {
        label: 'Partly Thinpost',
        tone: 'warning',
        detail: 'Some MX records name another mail host, so part of your incoming mail will go there instead of Thinpost.'
    },
    other: { label: 'Points elsewhere', tone: 'warning', detail: 'The MX records name another mail host, so incoming mail will not reach Thinpost.' },
    none: { label: 'No MX record', tone: 'warning', detail: 'The domain has no MX record, so senders have nowhere to deliver mail for it.' },
    null_mx: { label: 'Refuses mail', tone: 'danger', detail: 'The domain has a "null" MX record, which tells senders it accepts no mail.' },
    error: { label: 'Check failed', tone: 'warning', detail: 'The lookup did not finish, so the result is unknown. Try the check again.' }
};

const isString = (value: unknown, max = 512): value is string => typeof value === 'string' && value.length > 0 && value.length <= max;

function parseDomain(value: unknown): InboundDomain {
    const d = value as Partial<InboundDomain> | null | undefined;
    if (!d || !isString(d.name, 253) || !isString(d.created_at, 64) || !(d.state === 'pending' || d.state === 'active' || d.state === 'disabled')) {
        throw new InvalidResponseError();
    }
    const out: InboundDomain = { name: d.name, state: d.state, created_at: d.created_at };
    if (d.verified_at !== undefined) {
        if (!isString(d.verified_at, 64)) throw new InvalidResponseError();
        out.verified_at = d.verified_at;
    }
    if (d.mx_status !== undefined) {
        if (!Object.hasOwn(MX_DESCRIPTIONS, d.mx_status) || !isString(d.mx_checked_at, 64)) throw new InvalidResponseError();
        out.mx_status = d.mx_status;
        out.mx_checked_at = d.mx_checked_at;
    }
    return out;
}

/** Validates one domain as returned by the verify, check, disable and enable calls. */
export function parseInboundDomain(data: unknown): InboundDomain {
    return parseDomain(data);
}

/** The listing and the host the MX record should name; the host is null when this deployment has none. */
export function parseInboundDomainList(data: unknown): { domains: InboundDomain[]; mxHost: string | null } {
    const candidate = data as { domains?: unknown; mx_host?: unknown } | null | undefined;
    if (!Array.isArray(candidate?.domains)) throw new InvalidResponseError();
    if (candidate.mx_host !== undefined && !isString(candidate.mx_host, 253)) throw new InvalidResponseError();
    return { domains: candidate.domains.map(parseDomain), mxHost: (candidate.mx_host as string | undefined) ?? null };
}

export interface InboundChallenge {
    domain: string;
    txtName: string;
    txtValue: string;
    /** Null when this deployment has no receiving mail host configured. */
    mxHost: string | null;
}

export function parseInboundChallenge(data: unknown, fallbackDomain: string): InboundChallenge {
    const c = data as { domain?: { name?: unknown }; dns_name?: unknown; dns_value?: unknown; mx_host?: unknown } | null | undefined;
    if (!isString(c?.dns_name) || !isString(c.dns_value) || (c.mx_host !== undefined && !isString(c.mx_host, 253))) throw new InvalidResponseError();
    return {
        domain: isString(c.domain?.name, 253) ? c.domain.name : fallbackDomain,
        txtName: c.dns_name,
        txtValue: c.dns_value,
        mxHost: (c.mx_host as string | undefined) ?? null
    };
}

export function parseInboundLimits(data: unknown): InboundLimits {
    const c = data as Partial<InboundLimits> | null | undefined;
    const ok = (value: unknown, max: number): value is number => typeof value === 'number' && Number.isInteger(value) && value >= 1 && value <= max;
    if (!ok(c?.daily_limit, MAX_INBOUND_DAILY) || !ok(c.pending_limit, MAX_INBOUND_PENDING)) throw new InvalidResponseError();
    return { daily_limit: c.daily_limit, pending_limit: c.pending_limit };
}

/** A whole number from 1 to `max`. */
export function validateInboundLimit(raw: string, max: number, what: string): FieldResult {
    const text = raw.trim();
    if (!text) return { value: null, error: `Enter ${what}.` };
    if (!/^\d+$/.test(text)) return { value: null, error: 'Enter a whole number, without separators or decimals.' };
    const value = Number(text);
    if (value < 1) return { value: null, error: 'Enter at least 1.' };
    if (value > max) return { value: null, error: `Enter at most ${formatCount(max)}.` };
    return { value, error: null };
}
