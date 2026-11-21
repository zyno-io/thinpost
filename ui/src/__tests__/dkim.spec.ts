import { describe, expect, it } from 'vitest';

import { parseDkimKey, splitTxtValue } from '@/dkim';

describe('splitTxtValue', () => {
    it('leaves a value that fits in one string alone', () => {
        expect(splitTxtValue('v=DKIM1; k=rsa; p=abc')).toBe('v=DKIM1; k=rsa; p=abc');
        expect(splitTxtValue('x'.repeat(255))).toBe('x'.repeat(255));
    });

    it('splits a longer value into quoted strings of at most 255 characters', () => {
        const value = 'a'.repeat(255) + 'b'.repeat(255) + 'c';
        expect(splitTxtValue(value)).toBe(`"${'a'.repeat(255)}" "${'b'.repeat(255)}" "c"`);
    });

    it('joins back to the original value', () => {
        const value = 'v=DKIM1; k=rsa; p=' + 'Q'.repeat(392);
        const parts = splitTxtValue(value).match(/"([^"]*)"/g)!.map(p => p.slice(1, -1));
        expect(parts.every(p => p.length <= 255)).toBe(true);
        expect(parts.join('')).toBe(value);
    });
});

describe('parseDkimKey', () => {
    it('reads a key and whether its record was seen', () => {
        expect(parseDkimKey({ selector: 's', dns_name: 'n', dns_value: 'v' })).toEqual({ selector: 's', dnsName: 'n', dnsValue: 'v', published: false });
        expect(parseDkimKey({ selector: 's', dns_name: 'n', dns_value: 'v', published_at: '2026-10-04T12:00:00Z' })?.published).toBe(true);
    });

    it('rejects anything that is not a complete key', () => {
        for (const value of [undefined, null, 'x', {}, { selector: 's', dns_name: 'n' }, { selector: '', dns_name: 'n', dns_value: 'v' }]) {
            expect(parseDkimKey(value)).toBeNull();
        }
    });
});
