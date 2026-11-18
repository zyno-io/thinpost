import type { Suppression } from '@/openapi-client-generated';
import type { StateLabel } from '@/states';

export type SuppressionReason = Suppression['reason'];

export const SUPPRESSION_REASONS: readonly SuppressionReason[] = ['hard_bounce', 'spam_complaint', 'unsubscribe', 'manual'];

export const REASON_LABELS: Record<string, { label: string; tone: StateLabel['tone'] }> = {
    hard_bounce: { label: 'Bounced', tone: 'danger' },
    spam_complaint: { label: 'Spam complaint', tone: 'danger' },
    unsubscribe: { label: 'Unsubscribed', tone: 'warning' },
    manual: { label: 'Added manually', tone: 'inactive' }
};

export const ORIGIN_LABELS: Record<string, string> = { recipient: 'Recipient', api: 'API', console: 'Console' };

export const MAX_ADD = 100;
export const SEARCH_MAX_LENGTH = 254;

/** Unknown values are shown as a generic label, never echoed. */
export function reasonLabel(reason: unknown): { label: string; tone: StateLabel['tone'] } {
    return (typeof reason === 'string' && Object.hasOwn(REASON_LABELS, reason) ? REASON_LABELS[reason] : undefined) ?? { label: 'Unknown', tone: 'inactive' };
}

export function originLabel(origin: unknown): string {
    return typeof origin === 'string' && Object.hasOwn(ORIGIN_LABELS, origin) ? ORIGIN_LABELS[origin]! : 'Unknown';
}

export interface SuppressionQuery {
    q: string;
    field: 'address' | 'domain';
}

/** A leading `@` makes the search an exact-domain match; otherwise it is an address prefix. Null when empty. */
export function parseSuppressionQuery(raw: string): SuppressionQuery | null {
    const text = raw.trim();
    if (text.startsWith('@')) {
        const domain = text.slice(1).trim();
        return domain ? { q: domain, field: 'domain' } : null;
    }
    return text ? { q: text, field: 'address' } : null;
}

const PART = /^[^\s@<>(),;:"[\]\\]+$/;
const CONTROL = /[\u0000-\u001f\u007f]/;
const encoder = new TextEncoder();

/** Stricter than a typical form check so it matches the server's mailbox parser: no empty dot segments, no control characters. */
export function isValidAddress(address: string): boolean {
    if (CONTROL.test(address) || encoder.encode(address).length > 998) return false;
    const at = address.lastIndexOf('@');
    if (at <= 0 || at === address.length - 1) return false;
    const local = address.slice(0, at);
    const domain = address.slice(at + 1);
    if (!PART.test(local) || !PART.test(domain)) return false;
    if (local.startsWith('.') || local.endsWith('.') || local.includes('..')) return false;
    if (domain.startsWith('.') || domain.endsWith('.') || domain.includes('..') || !domain.includes('.')) return false;
    return true;
}

export interface ParsedAddresses {
    valid: string[];
    invalid: number;
    duplicates: number;
}

/** One address per line. Blank lines are ignored; case-insensitive repeats are dropped and counted. */
export function parseAddresses(text: string): ParsedAddresses {
    const valid: string[] = [];
    const seen = new Set<string>();
    let invalid = 0;
    let duplicates = 0;
    for (const line of text.split(/\r?\n/)) {
        const address = line.trim();
        if (!address) continue;
        if (!isValidAddress(address)) {
            invalid++;
            continue;
        }
        const key = address.toLowerCase();
        if (seen.has(key)) {
            duplicates++;
            continue;
        }
        seen.add(key);
        valid.push(address);
    }
    return { valid, invalid, duplicates };
}
