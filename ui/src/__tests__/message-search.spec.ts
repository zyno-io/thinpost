import { describe, expect, it } from 'vitest';

import {
    excludesProtected,
    isDomainField,
    isExactMessageId,
    normalizeMessageId,
    normalizeSearch,
    recipientDomain,
    searchValidationHint,
    type MessageSearch
} from '@/message-search';

describe('normalizeSearch', () => {
    it('trims and defaults an unknown field', () => {
        expect(normalizeSearch({ q: '  Ada  ', field: 'bogus' as MessageSearch['field'] })).toEqual({ q: 'Ada', field: 'any' });
    });
    it('lowercases ids and domains and strips one leading @ and a trailing dot', () => {
        expect(normalizeSearch({ q: 'ABCDEF12', field: 'id' }).q).toBe('abcdef12');
        expect(normalizeSearch({ q: '@Example.COM.', field: 'sender_domain' }).q).toBe('example.com');
        expect(normalizeSearch({ q: '@@x.example', field: 'recipient_domain' }).q).toBe('@x.example');
    });
    it('keeps case for free text fields', () => {
        expect(normalizeSearch({ q: 'Ada@X.example', field: 'recipient' }).q).toBe('Ada@X.example');
    });
});

describe('searchValidationHint', () => {
    const hint = (q: string, field: MessageSearch['field']) => searchValidationHint(normalizeSearch({ q, field }));
    it('validates id prefixes', () => {
        expect(hint('abc1234', 'id')).toContain('8 to 32 characters (0-9, a-f)');
        expect(hint('a'.repeat(33), 'id')).toContain('8 to 32 characters (0-9, a-f)');
        expect(hint('xyz12345', 'id')).toContain('8 to 32 characters (0-9, a-f)');
        expect(hint('a'.repeat(8), 'id')).toBeNull();
        expect(hint('a'.repeat(32), 'id')).toBeNull();
    });
    it('validates domain fields with the domain rules', () => {
        expect(hint('localhost', 'sender_domain')).toContain('full domain name');
        expect(hint('192.0.2.4', 'recipient_domain')).toContain('IP address');
        expect(hint('bad..com', 'sender_domain')).toContain('two dots in a row');
        expect(hint('@Example.com', 'sender_domain')).toBeNull();
    });
    it('mirrors the server byte and control-character limits', () => {
        expect(hint('é'.repeat(127), 'subject')).toBeNull();
        expect(hint('é'.repeat(128), 'subject')).toContain('254 characters');
        expect(hint('a\tb', 'subject')).toContain('control characters');
        expect(hint('a\u0085b', 'any')).toContain('control characters');
    });
    it('has no hint for empty or free text', () => {
        expect(hint('', 'id')).toBeNull();
        expect(hint('anything', 'any')).toBeNull();
    });
});

describe('helpers', () => {
    it('recipientDomain', () => {
        expect(recipientDomain('Ada@Example.COM')).toBe('example.com');
        expect(recipientDomain('nobody')).toBeNull();
        expect(recipientDomain('a@')).toBeNull();
    });
    it('isExactMessageId', () => {
        expect(isExactMessageId('a'.repeat(32))).toBe(true);
        expect(isExactMessageId('a'.repeat(31))).toBe(false);
        expect(isExactMessageId('A'.repeat(32))).toBe(true);
    });
    it('normalizeMessageId accepts every provider rendering', () => {
        const id = '0123456789abcdef0123456789abcdef';
        for (const form of [id, '01234567-89AB-cdef-0123-456789abcdef', `${id}@example.com`, `<${id}@Example.com>`, ` ${id} `]) {
            expect(normalizeMessageId(form)).toBe(id);
        }
        for (const bad of ['', 'abc', id + '0', '01234567-89ab-cdef-0123', `@example.com`, `${id}@`, `${id}@a@b`, `<${id}`, 'z'.repeat(32)]) {
            expect(normalizeMessageId(bad)).toBeNull();
        }
    });
    it('id search validation and normalization accept full provider IDs', () => {
        const id = '0123456789abcdef0123456789abcdef';
        for (const form of ['01234567-89ab-cdef-0123-456789abcdef', `<${id}@example.com>`]) {
            const normalized = normalizeSearch({ q: form, field: 'id' });
            expect(normalized.q).toBe(id);
            expect(searchValidationHint(normalized)).toBeNull();
        }
        expect(normalizeSearch({ q: `${id}@example.com`, field: 'any' }).q).toBe(id);
        expect(searchValidationHint(normalizeSearch({ q: '01234567-89ab-cdef-0123', field: 'id' }))).not.toBeNull();
    });
    it('excludesProtected and isDomainField', () => {
        expect(excludesProtected('any')).toBe(true);
        expect(excludesProtected('subject')).toBe(true);
        expect(excludesProtected('id')).toBe(false);
        expect(excludesProtected('sender_domain')).toBe(false);
        expect(isDomainField('sender_domain')).toBe(true);
        expect(isDomainField('recipient_domain')).toBe(true);
        expect(isDomainField('recipient')).toBe(false);
    });
});
