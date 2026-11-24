import { formatDateTime } from '@/format';
import type { Domain } from '@/openapi-client-generated';

export interface RemovedDkimRecord {
    selector: string;
    dnsName: string;
    /** When the record may go; null means the key never signed, so it can be deleted now. */
    keepUntil: string | null;
}

/** What removing a sender domain left in DNS, as the API sent it. */
export interface DomainRemovalView {
    name: string;
    removedAt: string;
    dkimRecords: RemovedDkimRecord[];
    bounceDomain: string | null;
    trackingDomain: string | null;
    keepRecordsUntil: string | null;
}

export interface CleanupRow {
    id: string;
    /** The DNS name. */
    name: string;
    /** What to do with it, with any date already formatted. */
    advice: string;
}

function isText(value: unknown): value is string {
    return typeof value === 'string' && value.length > 0;
}

function isDate(value: unknown): value is string {
    return typeof value === 'string' && !Number.isNaN(new Date(value).getTime());
}

/** Reads an optional string the API leaves out when it has nothing to say; undefined (invalid) when present but malformed. */
function optional(value: unknown, valid: (v: unknown) => boolean): string | null | undefined {
    if (value === undefined || value === null) return null;
    return valid(value) ? (value as string) : undefined;
}

/** Reads the removal receipt; null when it does not match the contract. */
export function parseDomainRemoval(data: unknown): DomainRemovalView | null {
    if (typeof data !== 'object' || data === null) return null;
    const body = data as Record<string, unknown>;
    if (!isText(body.name) || !isDate(body.removed_at) || !Array.isArray(body.dkim_records)) return null;

    const dkimRecords: RemovedDkimRecord[] = [];
    for (const item of body.dkim_records) {
        if (typeof item !== 'object' || item === null) return null;
        const record = item as Record<string, unknown>;
        const keepUntil = optional(record.keep_until, isDate);
        if (!isText(record.selector) || !isText(record.dns_name) || keepUntil === undefined) return null;
        dkimRecords.push({ selector: record.selector, dnsName: record.dns_name, keepUntil });
    }

    const bounceDomain = optional(body.bounce_domain, isText);
    const trackingDomain = optional(body.tracking_domain, isText);
    const keepRecordsUntil = optional(body.keep_records_until, isDate);
    if (bounceDomain === undefined || trackingDomain === undefined || keepRecordsUntil === undefined) return null;

    return { name: body.name, removedAt: body.removed_at, dkimRecords, bounceDomain, trackingDomain, keepRecordsUntil };
}

export interface DependentSubdomain {
    name: string;
    bounce: boolean;
    tracking: boolean;
}

/** Whether mail may use this domain's bounce domain: only an active one is used. */
function servesBounce(d: Domain): boolean {
    return d.bounce_domain?.state === 'active';
}

/** Whether mail may use this domain's tracking domain: an active one, on a verified sender domain. */
function servesTracking(d: Domain): boolean {
    return d.active && d.tracking_domain?.state === 'active';
}

/**
 * The app's verified subdomains of `domain` whose mail uses its bounce or tracking domain. Mail takes the one of
 * the most specific domain that has one, so a subdomain counts only when nothing between it and `domain` (itself
 * included) has its own. Once `domain` is removed, they fall back to a further parent's, or to Thinpost's standard
 * addresses.
 */
export function dependentSubdomains(domain: Domain, all: readonly Domain[]): DependentSubdomain[] {
    const hasBounce = servesBounce(domain);
    const hasTracking = servesTracking(domain);
    if (!hasBounce && !hasTracking) return [];
    const suffix = `.${domain.name}`;
    const result: DependentSubdomain[] = [];
    for (const other of all) {
        if (!other.active || !other.name.endsWith(suffix)) continue;
        const nearer = all.filter(d => d.name.endsWith(suffix) && (d.name === other.name || other.name.endsWith(`.${d.name}`)));
        const bounce = hasBounce && !nearer.some(servesBounce);
        const tracking = hasTracking && !nearer.some(servesTracking);
        if (bounce || tracking) result.push({ name: other.name, bounce, tracking });
    }
    return result;
}

/** The DNS records to clean up after a removal, in the order the notice lists them. */
export function cleanupRecords(removal: DomainRemovalView): CleanupRow[] {
    const rows: CleanupRow[] = removal.dkimRecords.map(record => ({
        id: `dkim-${record.selector}`,
        name: record.dnsName,
        advice: record.keepUntil ? `keep until ${formatDateTime(record.keepUntil)}` : 'never used, delete now'
    }));
    if (removal.bounceDomain) {
        rows.push({
            id: 'bounce',
            name: removal.bounceDomain,
            advice: removal.keepRecordsUntil ? `delete the CNAME after ${formatDateTime(removal.keepRecordsUntil)}` : 'delete the CNAME'
        });
    }
    if (removal.trackingDomain) {
        rows.push({ id: 'tracking', name: removal.trackingDomain, advice: 'keep its DNS and certificate while old links matter' });
    }
    return rows;
}
