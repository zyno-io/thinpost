import { InvalidResponseError } from './api-errors';
import type { AppSendingIp, SendingIp, SendingIpDnsCheck, SendingIps } from './openapi-client-generated';

/** The sending IP name a person sees for an app that has no choice. */
export const MAIN_IP_LABEL = 'Main IP';

export const MAX_SENDING_IPS = 64;

const ID = /^[0-9a-f]{32}$/;

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value);
const isTime = (value: unknown): value is string => typeof value === 'string' && value.length <= 64 && !Number.isNaN(new Date(value).getTime());
const isNullableBool = (value: unknown) => value === null || typeof value === 'boolean';

/** An IP is shown by its HELO name and address, for example `mx.example.net (203.0.113.10)`. */
export function ipLabel(ip: Pick<SendingIp, 'helo_name' | 'address'>): string {
    return `${ip.helo_name} (${ip.address})`;
}

/** An app's choice: all three fields are null for no choice, and all set otherwise. */
export function isAppSendingIp(data: unknown): data is AppSendingIp {
    if (!isRecord(data)) return false;
    const { sending_ip_id: id, address, helo_name: helo } = data;
    if (id === null) return address === null && helo === null;
    return typeof id === 'string' && ID.test(id) && typeof address === 'string' && address.length <= 64 && typeof helo === 'string' && helo.length <= 253;
}

/** Checks the response shape so a stale or foreign body never renders as an empty list. */
export function parseSendingIps(data: unknown): SendingIps {
    if (
        !isRecord(data) ||
        !Array.isArray(data.ips) ||
        data.ips.length > MAX_SENDING_IPS ||
        !Number.isInteger(data.unpinned_app_count) ||
        (data.unpinned_app_count as number) < 0 ||
        !(data.edge_reported_at === null || isTime(data.edge_reported_at)) ||
        typeof data.edge_reports_identity !== 'boolean' ||
        !(data.edge_primary_address === null || typeof data.edge_primary_address === 'string')
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
        typeof data.address !== 'string' ||
        data.address.length > 64 ||
        typeof data.helo_name !== 'string' ||
        data.helo_name.length > 253 ||
        !isTime(data.created_at) ||
        !Number.isInteger(data.app_count) ||
        (data.app_count as number) < 0 ||
        typeof data.is_primary !== 'boolean' ||
        !isNullableBool(data.on_edge)
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
        !['reverse', 'forward', 'mx', 'spf'].every(k => typeof data[k] === 'string' && STATUSES.includes(data[k] as string))
    ) {
        throw new InvalidResponseError();
    }
    return data as unknown as SendingIpDnsCheck;
}

// --- Display ---

export type OnWorker = 'yes' | 'no' | 'unknown';

export function onWorker(ip: Pick<SendingIp, 'on_edge'>): OnWorker {
    return ip.on_edge === null ? 'unknown' : ip.on_edge ? 'yes' : 'no';
}

/** "mail from N apps is waiting" for an IP the sending worker no longer has; empty when all is well. */
export function workerNote(ip: Pick<SendingIp, 'on_edge' | 'app_count'>): string {
    if (ip.on_edge !== false) return '';
    if (ip.app_count === 0) return 'The sending worker no longer has this IP.';
    const apps = ip.app_count === 1 ? '1 app' : `${ip.app_count} apps`;
    return `The sending worker no longer has this IP, so mail from ${apps} is waiting. Move ${ip.app_count === 1 ? 'it' : 'them'} to another IP. Mail is never sent from a different address.`;
}

/** Apps whose mail leaves from this row: those pinned to it, plus those with no choice when it is the main IP. */
export function appsUsing(ip: Pick<SendingIp, 'app_count' | 'is_primary'>, unpinned: number): number {
    return ip.app_count + (ip.is_primary ? unpinned : 0);
}

export const DNS_CHECKS = [
    { key: 'reverse', label: 'Reverse DNS names this IP by its HELO name' },
    { key: 'forward', label: 'The HELO name points to this IP' },
    { key: 'mx', label: 'Mail for the HELO name (bounces) reaches this IP' },
    { key: 'spf', label: 'The HELO name\'s SPF record lists this IP' }
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

/** What to ask DNS staff for, for each check that isn't OK. */
export function dnsHints(ip: Pick<SendingIp, 'address' | 'helo_name'>, check: SendingIpDnsCheck): string[] {
    const hints: string[] = [];
    if (check.reverse === 'mismatch' || check.reverse === 'missing') hints.push(`Set the reverse DNS (PTR record) of ${ip.address} to ${ip.helo_name}.`);
    if (check.forward === 'mismatch' || check.forward === 'missing') hints.push(`Point ${ip.helo_name} to ${ip.address} with an A record.`);
    if (check.mx === 'mismatch' || check.mx === 'missing') hints.push(`Add an MX record for ${ip.helo_name} that points to ${ip.helo_name}, so bounces come back to this IP.`);
    if (check.spf === 'mismatch' || check.spf === 'missing') hints.push(`Give ${ip.helo_name} a TXT record such as "v=spf1 ip4:${ip.address} -all" (or "v=spf1 a -all" once its A record points to this IP), keeping anything it already lists.`);
    return hints;
}

/** An SPF record that lists the IP through an include isn't followed, so a mismatch there can be a false alarm. */
export const SPF_CAVEAT = 'Only ip4: and a entries are read. An address listed through an include: or redirect shows as not matching.';
