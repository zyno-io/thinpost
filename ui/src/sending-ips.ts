import { InvalidResponseError } from './api-errors';
import type { SendingIp, SendingIpDnsCheck, SendingIps } from './openapi-client-generated';
import { normalizeDomain } from './validation';

/** The sending IP name a person sees for an app that has no assignment. */
export const MAIN_IP_LABEL = 'Main IP';

export const MAX_SENDING_IPS = 64;

const NAME = /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/;
const RESERVED = new Set(['primary', 'other', 'removed']);
const ID = /^[0-9a-f]{32}$/;

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value);
const isTime = (value: unknown): value is string => typeof value === 'string' && value.length <= 64 && !Number.isNaN(new Date(value).getTime());
const isNullableBool = (value: unknown) => value === null || typeof value === 'boolean';

/** Checks the response shape so a stale or foreign body never renders as an empty pool. */
export function parseSendingIps(data: unknown): SendingIps {
    if (
        !isRecord(data) ||
        !Array.isArray(data.ips) ||
        data.ips.length > MAX_SENDING_IPS ||
        !Number.isInteger(data.unpinned_app_count) ||
        (data.unpinned_app_count as number) < 0 ||
        !(data.edge_reported_at === null || isTime(data.edge_reported_at)) ||
        typeof data.edge_reports_sources !== 'boolean' ||
        !(data.edge_primary_address === null || typeof data.edge_primary_address === 'string') ||
        !Array.isArray(data.unnamed_edge_addresses) ||
        data.unnamed_edge_addresses.length > 32 ||
        data.unnamed_edge_addresses.some(a => typeof a !== 'string' || a.length > 64) ||
        (data.global_bounce_domain !== undefined && typeof data.global_bounce_domain !== 'string')
    ) {
        throw new InvalidResponseError();
    }
    for (const ip of data.ips) parseSendingIp(ip);
    return data as unknown as SendingIps;
}

export function parseSendingIp(data: unknown): SendingIp {
    if (
        !isRecord(data) ||
        typeof data.id !== 'string' ||
        !ID.test(data.id) ||
        typeof data.name !== 'string' ||
        !NAME.test(data.name) ||
        typeof data.address !== 'string' ||
        data.address.length > 64 ||
        typeof data.helo_name !== 'string' ||
        data.helo_name.length > 253 ||
        typeof data.bounce_domain !== 'string' ||
        data.bounce_domain.length > 219 ||
        !isTime(data.created_at) ||
        !Number.isInteger(data.app_count) ||
        (data.app_count as number) < 0 ||
        typeof data.is_primary !== 'boolean' ||
        !isNullableBool(data.on_edge) ||
        !isNullableBool(data.bounce_on_edge)
    ) {
        throw new InvalidResponseError();
    }
    return data as unknown as SendingIp;
}

const STATUSES = ['ok', 'mismatch', 'missing', 'unavailable'];
export function parseDnsCheck(data: unknown): SendingIpDnsCheck {
    if (
        !isRecord(data) ||
        !Array.isArray(data.reverse_names) ||
        data.reverse_names.length > 8 ||
        data.reverse_names.some(n => typeof n !== 'string' || n.length > 253) ||
        !['reverse', 'forward', 'helo_spf', 'bounce_spf'].every(k => typeof data[k] === 'string' && STATUSES.includes(data[k] as string))
    ) {
        throw new InvalidResponseError();
    }
    return data as unknown as SendingIpDnsCheck;
}

// --- Input rules, mirroring the API. The server remains authoritative. ---

export function validateIpName(value: string): string | null {
    if (!value) return 'Enter a name.';
    if (RESERVED.has(value)) return `"${value}" is reserved. Choose another name.`;
    if (!NAME.test(value)) return 'Use 1 to 32 lowercase letters, digits and hyphens, starting and ending with a letter or digit.';
    return null;
}

/** A public IPv4 address as four plain numbers; the server also refuses private, loopback and reserved ranges. */
export function validateIpAddress(value: string): string | null {
    if (!value) return 'Enter an IPv4 address.';
    const parts = value.split('.');
    if (parts.length !== 4 || !parts.every(p => /^(0|[1-9]\d{0,2})$/.test(p) && Number(p) <= 255)) return 'Enter an IPv4 address such as 203.0.113.10.';
    const [a = 0, b = 0] = parts.map(Number);
    if (a === 0 || a === 10 || a === 127 || a >= 224 || (a === 169 && b === 254) || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168)) {
        return 'Use a public address, not a private or reserved one.';
    }
    return null;
}

export function validateIpDomain(value: string, label: string, max: number): string | null {
    const normalized = normalizeDomain(value);
    if (normalized.error) return normalized.error;
    if (normalized.domain !== value) return `Use lowercase letters only, with no spaces, in the ${label}.`;
    if (value.length < 3 || value.length > max) return `Use at most ${max} characters.`;
    return null;
}

/** The bounce domain must be the HELO name or a domain it belongs to, so the sending worker accepts the bounces. */
export function validateBounceForHelo(helo: string, bounce: string): string | null {
    if (!helo || !bounce || helo === bounce || helo.endsWith(`.${bounce}`)) return null;
    return 'Use this IP\'s mail server name, or a domain it belongs to, as the bounce domain.';
}

// --- Display ---

export type OnWorker = 'yes' | 'no' | 'unknown';

export function onWorker(ip: Pick<SendingIp, 'on_edge'>): OnWorker {
    return ip.on_edge === null ? 'unknown' : ip.on_edge ? 'yes' : 'no';
}

/** One line of what an administrator should know about an IP's presence on the sending worker; empty when all is well. */
export function workerNote(ip: Pick<SendingIp, 'on_edge' | 'bounce_on_edge'>): string {
    if (ip.on_edge === false) return 'The sending worker doesn\'t have this address, so mail from apps that use it is waiting. It is never sent from another address.';
    if (ip.on_edge && ip.bounce_on_edge === false) return 'The sending worker doesn\'t accept this IP\'s bounce domain, so mail from apps that use it is waiting.';
    return '';
}

/** Apps whose mail leaves from this row: those pinned to it, plus those with no assignment when it is the main IP. */
export function appsUsing(ip: Pick<SendingIp, 'app_count' | 'is_primary'>, unpinned: number): number {
    return ip.app_count + (ip.is_primary ? unpinned : 0);
}

/** Why a row can't be deleted right now, or null. */
export function deleteBlocker(ip: Pick<SendingIp, 'app_count'>, gateMode: string | null, signedIn: boolean): string | null {
    if (!signedIn) return 'Sign in again to change sending IPs.';
    if (gateMode !== null && gateMode !== 'open') return 'Changes are only possible while mail intake is open.';
    if (ip.app_count > 0) return `${ip.app_count === 1 ? '1 app uses' : `${ip.app_count} apps use`} this IP. Move ${ip.app_count === 1 ? 'it' : 'them'} to another IP first.`;
    return null;
}

export function changeBlocker(gateMode: string | null, signedIn: boolean): string | null {
    if (!signedIn) return 'Sign in again to change sending IPs.';
    if (gateMode !== null && gateMode !== 'open') return 'Changes are only possible while mail intake is open.';
    return null;
}

export interface SpfGroup {
    domain: string;
    addresses: string[];
    /**
     * The record text to add: `a` when the domain is the HELO name of its only IP (the name already
     * points at the IP, so the record follows the address), otherwise one ip4: entry per address.
     */
    entries: string;
}

/** Sending IPs grouped by bounce domain: the SPF record of each domain must list exactly its own IPs. */
export function spfGroups(ips: readonly SendingIp[]): SpfGroup[] {
    const groups = new Map<string, SendingIp[]>();
    for (const ip of ips) groups.set(ip.bounce_domain, [...(groups.get(ip.bounce_domain) ?? []), ip]);
    return [...groups.entries()]
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([domain, members]) => ({
            domain,
            addresses: members.map(ip => ip.address),
            entries: members.length === 1 && members[0]!.helo_name === domain ? 'a' : members.map(ip => `ip4:${ip.address}`).join(' ')
        }));
}

export const DNS_CHECKS = [
    { key: 'reverse', label: 'Reverse DNS names this IP by its HELO name' },
    { key: 'forward', label: 'The HELO name points back to this IP' },
    { key: 'helo_spf', label: 'The HELO name has an SPF record' },
    { key: 'bounce_spf', label: 'The bounce domain\'s SPF record lists this IP' }
] as const;

type DnsTone = '' | 'success' | 'warning' | 'danger';
const DNS_STATUS_TEXT: Record<string, { text: string; tone: DnsTone }> = {
    ok: { text: 'OK', tone: 'success' },
    mismatch: { text: 'Doesn\'t match', tone: 'warning' },
    missing: { text: 'Not found', tone: 'warning' },
    unavailable: { text: 'Couldn\'t check', tone: '' }
};

/** The words and colour for one DNS check result; an unknown value reads as "couldn't check". */
export function dnsStatus(status: string): { text: string; tone: DnsTone } {
    return DNS_STATUS_TEXT[status] ?? { text: 'Couldn\'t check', tone: '' };
}

/** An SPF record that lists the IP through an include isn't followed, so a mismatch there can be a false alarm. */
export const BOUNCE_SPF_CAVEAT = 'Only ip4: and a entries are read. An address listed through an include: or redirect shows as not matching.';
