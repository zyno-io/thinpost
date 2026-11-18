import { nextTick } from 'vue';
import { beforeEach, describe, expect, it } from 'vitest';

import { authState } from '@/auth';
import { clearSavedSuppressionSearches, savedSuppressionSearch, saveSuppressionSearch } from '@/suppression-search-store';
import { originLabel, parseAddresses, parseSuppressionQuery, reasonLabel } from '@/suppressions';

describe('suppression helpers', () => {
    it('treats a leading @ as an exact domain search', () => {
        expect(parseSuppressionQuery(' ada@ex ')).toEqual({ q: 'ada@ex', field: 'address' });
        expect(parseSuppressionQuery('@Example.com')).toEqual({ q: 'Example.com', field: 'domain' });
        expect(parseSuppressionQuery('@')).toBeNull();
        expect(parseSuppressionQuery('  ')).toBeNull();
    });

    it('parses one address per line and counts invalid and duplicate entries', () => {
        const parsed = parseAddresses('a@example.com\n\n  B@example.org \nnot-an-address\nA@EXAMPLE.com\r\nName <x@y.example>');
        expect(parsed.valid).toEqual(['a@example.com', 'B@example.org']);
        expect(parsed.invalid).toBe(2);
        expect(parsed.duplicates).toBe(1);
    });

    it('labels reasons and origins and never echoes unknown values', () => {
        expect(reasonLabel('hard_bounce').label).toBe('Bounced');
        expect(reasonLabel('spam_complaint').label).toBe('Spam complaint');
        expect(reasonLabel('unsubscribe').label).toBe('Unsubscribed');
        expect(reasonLabel('manual').label).toBe('Added manually');
        expect(reasonLabel('<script>').label).toBe('Unknown');
        expect(originLabel('recipient')).toBe('Recipient');
        expect(originLabel('api')).toBe('API');
        expect(originLabel('console')).toBe('Console');
        expect(originLabel('x')).toBe('Unknown');
    });
});

describe('address validation', () => {
    it('rejects what the server mailbox parser rejects', () => {
        for (const bad of ['.a@example.com', 'a.@example.com', 'a..b@example.com', 'a@.example.com', 'a@example..com', 'a@example.com.', 'a@example', 'a\u0001@example.com', ' @example.com']) {
            expect(parseAddresses(bad).invalid, bad).toBe(1);
        }
        expect(parseAddresses('first.last+tag@sub.example.com').valid).toHaveLength(1);
    });

    it('measures length in UTF-8 bytes', () => {
        const local = 'é'.repeat(495); // 990 bytes, 1002 with the domain
        expect(parseAddresses(`${local}@example.com`).invalid).toBe(1);
        expect(parseAddresses(`${'a'.repeat(985)}@example.com`).valid).toHaveLength(1);
    });
});

describe('suppression search store', () => {
    beforeEach(() => {
        authState.status = 'signed-in';
        clearSavedSuppressionSearches();
    });

    it('saves per app, deletes on empty, clears when signed out', async () => {
        saveSuppressionSearch('a', 'ada@');
        expect(savedSuppressionSearch('a')).toBe('ada@');
        expect(savedSuppressionSearch('b')).toBe('');
        saveSuppressionSearch('a', '');
        expect(savedSuppressionSearch('a')).toBe('');
        saveSuppressionSearch('a', '@x.example');
        authState.status = 'signed-out';
        await nextTick();
        expect(savedSuppressionSearch('a')).toBe('');
    });

    it('never writes to browser storage', () => {
        saveSuppressionSearch('a', 'secret@example.com');
        expect(JSON.stringify({ ...localStorage })).not.toContain('secret@');
        expect(JSON.stringify({ ...sessionStorage })).not.toContain('secret@');
    });
});
