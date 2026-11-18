const MAX_NAME_BYTES = 128;

// Go's unicode.IsSpace set, which strings.TrimSpace uses on the server. JS trim() differs (U+0085, U+FEFF).
const SPACE = '[\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000]';
const SURROUNDING_SPACE = new RegExp(`^${SPACE}|${SPACE}$`);

/**
 * Mirrors the operator API name contract: 1-128 UTF-8 bytes, no surrounding
 * whitespace and no control characters. The server remains authoritative.
 */
export function validateName(value: string): string | null {
    if (!value.length) return 'Enter a name.';
    if (SURROUNDING_SPACE.test(value)) return 'Remove leading or trailing whitespace.';
    // eslint-disable-next-line no-control-regex -- matches the server's C0/DEL rejection
    if (/[\u0000-\u001f\u007f]/.test(value)) return 'Remove control characters.';
    if (new TextEncoder().encode(value).length > MAX_NAME_BYTES) return `Use at most ${MAX_NAME_BYTES} bytes.`;
    return null;
}

export type DomainInput = { domain: string; error: null } | { domain: null; error: string };

/**
 * Mirrors the operator API's NormalizeDomain: an ASCII DNS name of at most 253
 * characters with at least two labels, each 1-63 of [a-z0-9-] without a
 * leading or trailing hyphen, and not an IP address. Input is lowercased as the
 * server does; surrounding whitespace is trimmed because it can never be part
 * of a valid name. The server remains authoritative.
 */
export function normalizeDomain(value: string): DomainInput {
    const domain = value.trim().toLowerCase();
    const fail = (error: string): DomainInput => ({ domain: null, error });
    if (!domain) return fail('Enter a domain.');
    // eslint-disable-next-line no-control-regex -- anything outside printable ASCII
    if (/[^\x21-\x7e]/.test(domain)) return fail('Use plain letters and digits. For international domain names, use the xn-- form.');
    if (domain.endsWith('.')) return fail('Remove the trailing dot.');
    if (domain.length > 253) return fail('Use at most 253 characters.');
    if (!domain.includes('.')) return fail('Enter a full domain name, such as mail.example.com.');
    if (/^\d+\.\d+\.\d+\.\d+$/.test(domain)) return fail('Enter a domain name, not an IP address.');
    for (const label of domain.split('.')) {
        if (!label) return fail('Remove the empty part (two dots in a row).');
        if (label.length > 63) return fail('Each part between dots can be at most 63 characters.');
        if (!/^[a-z0-9-]+$/.test(label)) return fail('Use only letters, digits, hyphens and dots.');
        if (label.startsWith('-') || label.endsWith('-')) return fail('Parts between dots can\'t start or end with a hyphen.');
    }
    return { domain, error: null };
}

export const MAX_WEBHOOK_ENDPOINT_BYTES = 2048;
export const MAX_WEBHOOK_DESCRIPTION_BYTES = 256;

// Names that resolve inside a cluster or host; the server rejects each as a host or as a parent domain.
const INTERNAL_SUFFIXES = ['localhost', 'local', 'internal', 'svc', 'home.arpa'];

/** The final label of a host that some resolvers read as an IPv4 shorthand (127.1, 0x7f.1). */
function isNumericLabel(label: string): boolean {
    const digits = label.replace(/^0[xX]/, '');
    if (!digits) return label !== '';
    return digits !== label ? /^[0-9a-fA-F]+$/.test(digits) : /^[0-9]+$/.test(digits);
}

/**
 * Mirrors the operator API's NormalizeWebhookEndpoint: an https URL with a
 * public DNS host name on port 443, ASCII only and at most 2048 bytes, with no
 * user information, fragment or bare "?". A query string is allowed and must
 * use valid percent escapes. The server remains authoritative.
 */
export function validateWebhookEndpoint(value: string): string | null {
    if (!value.length) return 'Enter a webhook address.';
    if (new TextEncoder().encode(value).length > MAX_WEBHOOK_ENDPOINT_BYTES) return `Use at most ${MAX_WEBHOOK_ENDPOINT_BYTES} bytes.`;
    if (value.includes('\\')) return 'Remove backslashes; use forward slashes.';
    if (value.includes('#')) return 'Remove the # and everything after it.';
    // eslint-disable-next-line no-control-regex -- matches the server's rejection of controls, DEL and spaces
    if (/[\u0000- \u007f]/.test(value)) return 'Remove spaces and control characters; percent-encode them if they belong in the URL.';
    if (/[^\u0000-\u007f]/.test(value)) return 'Use plain ASCII only. For international host names, use the xn-- form, and percent-encode other characters.';

    const scheme = /^([A-Za-z][A-Za-z0-9+.-]*):\/\//.exec(value);
    if (!scheme || scheme[1]!.toLowerCase() !== 'https') return 'Use an https:// address.';
    const rest = value.slice(scheme[0].length);
    const authorityEnd = rest.search(/[/?]/);
    const authority = authorityEnd < 0 ? rest : rest.slice(0, authorityEnd);
    const afterAuthority = authorityEnd < 0 ? '' : rest.slice(authorityEnd);
    const queryStart = afterAuthority.indexOf('?');
    const query = queryStart < 0 ? null : afterAuthority.slice(queryStart + 1);

    if (authority.includes('@')) return 'Remove the user name or password.';
    if (query === '') return 'Remove the trailing “?” or add a query after it.';

    let host = authority;
    let port = '';
    if (authority.startsWith('[')) return 'Use a host name, not an IP address.';
    const colon = authority.lastIndexOf(':');
    if (colon >= 0) {
        host = authority.slice(0, colon);
        port = authority.slice(colon + 1);
        if (!/^[0-9]*$/.test(port) || (port !== '' && port !== '443')) return 'Use port 443, or leave the port out.';
    }

    host = host.toLowerCase();
    if (!host) return 'Enter a host name.';
    if (/^\d+\.\d+\.\d+\.\d+$/.test(host)) return 'Use a host name, not an IP address.';
    if (!host.includes('.')) return 'Enter a full host name, such as hooks.example.com.';
    if (host.length > 253) return 'Use a host name of at most 253 characters.';
    for (const label of host.split('.')) {
        if (!label) return 'Remove the empty part (two dots in a row) from the host name.';
        if (label.length > 63) return 'Each part of the host name between dots can be at most 63 characters.';
        if (!/^[a-z0-9-]+$/.test(label)) return 'Use only letters, digits, hyphens and dots in the host name.';
        if (label.startsWith('-') || label.endsWith('-')) return 'Parts of the host name between dots can\'t start or end with a hyphen.';
    }
    if (isNumericLabel(host.slice(host.lastIndexOf('.') + 1))) return 'Use a host name, not an IP address or a numeric shorthand such as 127.1.';
    if (INTERNAL_SUFFIXES.some(suffix => host === suffix || host.endsWith(`.${suffix}`))) {
        return 'Use a public host name. Names like localhost, .local, .internal, .svc and .home.arpa can\'t be reached.';
    }
    if (/%(?![0-9a-fA-F]{2})/.test(afterAuthority)) return 'Fix the “%” in the path or query: it must be followed by two hex digits.';
    // The server measures the normalized URL: Go's EscapedPath percent-encodes these path characters (3 bytes each).
    const path = queryStart < 0 ? afterAuthority : afterAuthority.slice(0, queryStart);
    const escapedExtra = (path.match(/["<>{}|^`]/g)?.length ?? 0) * 2;
    if (value.length + escapedExtra > MAX_WEBHOOK_ENDPOINT_BYTES) {
        return `Use at most ${MAX_WEBHOOK_ENDPOINT_BYTES} bytes after normalization; the characters " < > { } | ^ \` count as 3 bytes each in the path.`;
    }
    return null;
}

/** Mirrors the operator API description contract: optional, at most 256 UTF-8 bytes, no surrounding whitespace or control characters. */
export function validateWebhookDescription(value: string): string | null {
    if (!value.length) return null;
    if (SURROUNDING_SPACE.test(value)) return 'Remove leading or trailing whitespace.';
    // eslint-disable-next-line no-control-regex -- matches the server's C0/DEL rejection
    if (/[\u0000-\u001f\u007f]/.test(value)) return 'Remove control characters.';
    if (new TextEncoder().encode(value).length > MAX_WEBHOOK_DESCRIPTION_BYTES) return `Use at most ${MAX_WEBHOOK_DESCRIPTION_BYTES} bytes.`;
    return null;
}

const OPAQUE_ID = /^[0-9a-f]{32}$/;

export function isOpaqueId(value: unknown): value is string {
    return typeof value === 'string' && OPAQUE_ID.test(value);
}

export const MAX_WEBHOOK_HEADERS = 5;

// Mirrors the server's denied header names and prefixes (admin.ValidWebhookHeader); canonical form is compared case-insensitively.
const DENIED_HEADERS = new Set([
    'host',
    'content-type',
    'content-length',
    'content-encoding',
    'transfer-encoding',
    'connection',
    'keep-alive',
    'upgrade',
    'te',
    'trailer',
    'expect'
]);
const DENIED_HEADER_PREFIXES = ['proxy-', 'x-thinpost-', 'svix-', 'webhook-', 'x-pm-', 'x-twilio-'];

/**
 * Mirrors the server's custom header rules: an HTTP token name of at most 64
 * bytes without underscores, not reserved or a signature header (Authorization
 * only when Basic is also set), and a value of 1-1024 printable ASCII bytes.
 * An empty `value` is accepted when `valueOptional` (an unchanged stored value).
 */
export function validateWebhookHeader(name: string, value: string, basicSet: boolean, valueOptional = false): string | null {
    if (!name) return 'Enter a header name.';
    if (name.length > 64) return 'Use a header name of at most 64 characters.';
    if (name.includes('_')) return 'Header names cannot contain underscores.';
    if (!/^[A-Za-z0-9!#$%&'*+.^`|~-]+$/.test(name)) return 'Use letters, digits and - only in the header name.';
    const lower = name.toLowerCase();
    if (DENIED_HEADERS.has(lower) || DENIED_HEADER_PREFIXES.some(prefix => lower.startsWith(prefix)) || (basicSet && lower === 'authorization')) {
        return 'This header name is reserved and can\'t be used.';
    }
    if (!value) return valueOptional ? null : 'Enter a header value.';
    if (value.length > 1024) return 'Use a header value of at most 1024 characters.';
    if (/[^ -~]/.test(value)) return 'Use printable ASCII in the header value.';
    return null;
}

/** Mirrors the server's Basic rules: user name 1-256 bytes without a colon, password 1-256 bytes, no control characters; both or neither. */
export function validateWebhookBasic(username: string, password: string, passwordOptional = false): { username: string | null; password: string | null } {
    const bytes = (s: string) => new TextEncoder().encode(s).length;
    // eslint-disable-next-line no-control-regex -- matches the server's rejection of C0, DEL and C1 controls
    const control = /[\u0000-\u001f\u007f-\u009f]/;
    let userError: string | null = null;
    let passError: string | null = null;
    if (!username && !password) return { username: null, password: null };
    if (!username) userError = 'Enter a user name.';
    else if (username.includes(':')) userError = 'The user name cannot contain a colon.';
    else if (bytes(username) > 256) userError = 'Use at most 256 bytes.';
    else if (control.test(username)) userError = 'Remove control characters.';
    if (!password) passError = passwordOptional ? null : 'Enter a password.';
    else if (bytes(password) > 256) passError = 'Use at most 256 bytes.';
    else if (control.test(password)) passError = 'Remove control characters.';
    return { username: userError, password: passError };
}

/**
 * Moves credentials pasted as `https://user:pass@host/…` out of the URL. Returns the URL without user
 * information and the percent-decoded credentials, or null when there is nothing to move.
 */
export function splitEndpointUserinfo(value: string): { endpoint: string; username: string; password: string } | null {
    const match = /^(https:\/\/)([^/?#\s]*)@([^/?#@\s]*)(.*)$/i.exec(value.trim());
    if (!match) return null;
    const userinfo = match[2]!;
    const colon = userinfo.indexOf(':');
    const decode = (s: string) => {
        try {
            return decodeURIComponent(s);
        } catch {
            return s;
        }
    };
    const username = decode(colon < 0 ? userinfo : userinfo.slice(0, colon));
    const password = colon < 0 ? '' : decode(userinfo.slice(colon + 1));
    return { endpoint: `${match[1]}${match[3]}${match[4]}`, username, password };
}
