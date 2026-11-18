import { describe, expect, it } from 'vitest';

import { InvalidResponseError } from '@/api-errors';
import { parseInboundChallenge, parseInboundDomain, parseInboundDomainList, parseInboundLimits, validateInboundLimit } from '@/inbound';

const base = { name: 'in.example.com', state: 'active', created_at: '2026-10-02T12:00:00Z' };

describe('inbound parsing', () => {
    it('reads a listing and the optional mail host', () => {
        expect(parseInboundDomainList({ domains: [base], mx_host: 'smtp.mx.example.net' })).toEqual({ domains: [base], mxHost: 'smtp.mx.example.net' });
        expect(parseInboundDomainList({ domains: [] }).mxHost).toBeNull();
    });

    it('requires the MX check time with the MX status', () => {
        expect(() => parseInboundDomain({ ...base, mx_status: 'thinpost' })).toThrow(InvalidResponseError);
        expect(parseInboundDomain({ ...base, mx_status: 'null_mx', mx_checked_at: '2026-10-04T09:30:00Z' }).mx_status).toBe('null_mx');
    });

    it.each([
        [{ ...base, state: 'bogus' }],
        [{ ...base, name: '' }],
        [{ ...base, mx_status: 'constructor', mx_checked_at: 'x' }],
        [null],
        ['x']
    ])('rejects a malformed domain %#', value => {
        expect(() => parseInboundDomain(value)).toThrow(InvalidResponseError);
    });

    it('rejects a listing that is not an array', () => {
        expect(() => parseInboundDomainList({ domains: {} })).toThrow(InvalidResponseError);
        expect(() => parseInboundDomainList({ domains: [], mx_host: 5 })).toThrow(InvalidResponseError);
    });

    it('requires both challenge fields', () => {
        expect(parseInboundChallenge({ dns_name: 'a', dns_value: 'b' }, 'in.example.com')).toEqual({ domain: 'in.example.com', txtName: 'a', txtValue: 'b', mxHost: null });
        expect(() => parseInboundChallenge({ dns_name: 'a' }, 'in.example.com')).toThrow(InvalidResponseError);
    });

    it('reads limits within their ranges only', () => {
        expect(parseInboundLimits({ daily_limit: 1, pending_limit: 100000 })).toEqual({ daily_limit: 1, pending_limit: 100000 });
        for (const bad of [{ daily_limit: 0, pending_limit: 5 }, { daily_limit: 5, pending_limit: 100001 }, { daily_limit: 1.5, pending_limit: 5 }, { daily_limit: 5 }, null]) {
            expect(() => parseInboundLimits(bad)).toThrow(InvalidResponseError);
        }
    });
});

describe('inbound limit validation', () => {
    it('accepts whole numbers from 1 to the maximum', () => {
        expect(validateInboundLimit(' 2000 ', 1_000_000, 'x')).toEqual({ value: 2000, error: null });
        expect(validateInboundLimit('1', 5, 'x').value).toBe(1);
        expect(validateInboundLimit('5', 5, 'x').value).toBe(5);
    });

    it.each(['', '0', '6', '1.5', '-1', '1,000', 'abc', '1e3'])('rejects %j', raw => {
        const result = validateInboundLimit(raw, 5, 'a number');
        expect(result.value).toBeNull();
        expect(result.error).toBeTruthy();
    });
});
