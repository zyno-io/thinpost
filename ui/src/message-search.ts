import { normalizeDomain } from '@/validation';

export const SEARCH_FIELDS = ['any', 'id', 'recipient', 'recipient_domain', 'sender_domain', 'subject'] as const;
export type SearchField = (typeof SEARCH_FIELDS)[number];

export interface MessageSearch {
    q: string;
    field: SearchField;
}

export const SEARCH_MAX_LENGTH = 254;
export const SEARCH_DEBOUNCE_MS = 300;

export function isDomainField(field: SearchField): boolean {
    return field === 'recipient_domain' || field === 'sender_domain';
}

/** Subject search never matches Enhanced Protection messages. */
export function excludesProtected(field: SearchField): boolean {
    return field === 'any' || field === 'subject';
}

/**
 * The 32-hex message ID for any provider rendering: bare hex, UUID text,
 * `id@domain` or `<id@domain>`. Mirrors providerid.Normalize on the server.
 */
export function normalizeMessageId(value: string): string | null {
    let s = value.trim();
    if (s.startsWith('<') && s.endsWith('>')) s = s.slice(1, -1);
    const at = s.indexOf('@');
    if (at >= 0) {
        if (at === 0 || at === s.length - 1 || s.indexOf('@', at + 1) >= 0) return null;
        s = s.slice(0, at);
    }
    s = s.toLowerCase();
    if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(s)) s = s.replaceAll('-', '');
    return /^[0-9a-f]{32}$/.test(s) ? s : null;
}

export function isExactMessageId(q: string): boolean {
    return normalizeMessageId(q) !== null;
}

/** Text after the last `@`, lowercased; null when there is none. */
export function recipientDomain(address: string): string | null {
    const at = address.lastIndexOf('@');
    if (at < 0 || at === address.length - 1) return null;
    return address.slice(at + 1).toLowerCase();
}

export const FIELD_COPY: Record<SearchField, { label: string; placeholder: string; hint: string }> = {
    any: {
        label: 'Any field',
        placeholder: 'Message ID, recipient, domain or subject words',
        hint: 'Matches the start of a message or delivery ID, the start of a recipient address, a recipient or sender domain, or words from the subject.'
    },
    id: { label: 'Message ID', placeholder: '8-32 characters, or a full message ID', hint: 'The start of a message or delivery ID, the full message ID your app got back from the Postmark, Resend or Mailgun-compatible API (UUID or <id@domain>), or a Message-ID header.' },
    recipient: {
        label: 'Recipient',
        placeholder: 'ada@ or @example.com',
        hint: 'The start of a recipient address, or a domain such as @example.com. Capitals don\'t matter.'
    },
    recipient_domain: { label: 'Recipient domain', placeholder: 'example.com', hint: 'Messages to recipients at this domain or its subdomains.' },
    sender_domain: { label: 'Sender domain', placeholder: 'example.com', hint: 'Messages sent from this domain or its subdomains.' },
    subject: { label: 'Subject', placeholder: 'Words from the subject', hint: 'Each word you enter (up to 8) must match the start of a word in the subject.' }
};

export const PROTECTED_SEARCH_NOTE = 'Subjects of Enhanced Protection messages can\'t be searched.';
export const PROTECTED_SEARCH_NOTE_ANY = `${PROTECTED_SEARCH_NOTE} They can still be found by message or delivery ID, recipient, recipient domain or sender domain.`;

/** Trims, defaults an unknown field to `any`, and canonicalizes ID and domain values. */
export function normalizeSearch(input: MessageSearch): MessageSearch {
    const field: SearchField = (SEARCH_FIELDS as readonly string[]).includes(input.field) ? input.field : 'any';
    let q = input.q.trim();
    if (field === 'id') q = q.toLowerCase();
    // A full ID in a provider's shape (UUID text, id@domain) is the same message ID.
    if (field === 'id' || field === 'any') q = normalizeMessageId(q) ?? q;
    if (isDomainField(field)) {
        q = q.toLowerCase();
        if (q.startsWith('@')) q = q.slice(1);
        if (q.endsWith('.')) q = q.slice(0, -1);
    }
    return { q, field };
}

/** A client-side reason the search cannot be sent, or null. Expects a normalized search. */
export function searchValidationHint(s: MessageSearch): string | null {
    if (!s.q) return null;
    // Mirrors the server's ValidSearchText: Unicode control characters (C0, DEL, C1) and a 254-byte UTF-8 cap.
    // eslint-disable-next-line no-control-regex
    if (/[\u0000-\u001f\u007f-\u009f]/.test(s.q)) return 'Remove tabs, line breaks and other control characters.';
    if (new TextEncoder().encode(s.q).length > SEARCH_MAX_LENGTH) return `Use at most ${SEARCH_MAX_LENGTH} characters. Accented and non-Latin characters can count as more than one.`;
    if (s.field === 'id' && !/^[0-9a-f]{8,32}$/i.test(s.q)) return 'Enter 8 to 32 characters (0-9, a-f) from the start of a message or delivery ID.';
    if (isDomainField(s.field)) {
        const result = normalizeDomain(s.q);
        if (result.error) return result.error;
    }
    return null;
}
