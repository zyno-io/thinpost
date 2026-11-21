/** Longest string a single DNS TXT string can hold. A longer value must be split into several strings. */
export const TXT_STRING_LIMIT = 255;

/**
 * Splits a TXT value into quoted strings of at most 255 characters, separated by spaces
 * (`"first" "second"`). DNS providers that cap a TXT string at 255 characters accept this form
 * and join the strings back into one value. A value that already fits comes back unchanged.
 */
export function splitTxtValue(value: string, limit = TXT_STRING_LIMIT): string {
    if (value.length <= limit) return value;
    const parts: string[] = [];
    for (let i = 0; i < value.length; i += limit) parts.push(`"${value.slice(i, i + limit)}"`);
    return parts.join(' ');
}

/** Reads a DKIM key as the API sends it; null when it is absent or malformed. */
export interface DkimKeyView {
    selector: string;
    dnsName: string;
    dnsValue: string;
    published: boolean;
}

export function parseDkimKey(value: unknown): DkimKeyView | null {
    if (typeof value !== 'object' || value === null) return null;
    const key = value as Record<string, unknown>;
    if (typeof key.selector !== 'string' || typeof key.dns_name !== 'string' || typeof key.dns_value !== 'string' || !key.selector || !key.dns_name || !key.dns_value) {
        return null;
    }
    return { selector: key.selector, dnsName: key.dns_name, dnsValue: key.dns_value, published: typeof key.published_at === 'string' };
}
