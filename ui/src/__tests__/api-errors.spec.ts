import { describe, expect, it } from 'vitest';

import { describeApiError, InvalidResponseError } from '../api-errors';
import { normalizeDomain, validateName } from '../validation';
import { httpError, networkError } from './helpers';

describe('describeApiError', () => {
    it.each([
        [400, 'invalid'],
        [401, 'unauthenticated'],
        [403, 'forbidden'],
        [404, 'not-found'],
        [503, 'unavailable'],
        [500, 'failed']
    ])('maps HTTP %i to %s', (status, kind) => {
        expect(describeApiError(httpError(status)).kind).toBe(kind);
    });

    it('treats a missing response as unreachable', () => {
        expect(describeApiError(networkError()).kind).toBe('unreachable');
        expect(describeApiError(new Error('boom')).kind).toBe('unreachable');
    });

    it('states that a mutation may not have gone through without a definitive response', () => {
        expect(describeApiError(networkError(), { mutation: true }).message).toMatch(/can't tell whether the change went through/);
        expect(describeApiError(httpError(503), { mutation: true }).message).toMatch(/can't tell whether the change went through/);
        expect(describeApiError(httpError(400), { mutation: true }).message).not.toMatch(/can't tell whether the change went through/);
    });

    it('uses the supplied not-found message', () => {
        expect(describeApiError(httpError(404), { notFound: 'Gone.' }).message).toBe('Gone.');
    });

    it('uses supplied conflict and unavailable messages', () => {
        expect(describeApiError(httpError(409)).kind).toBe('conflict');
        expect(describeApiError(httpError(409), { conflict: 'Taken.' }).message).toBe('Taken.');
        expect(describeApiError(httpError(503), { unavailable: 'DNS down.' }).message).toBe('DNS down.');
        expect(describeApiError(httpError(503), { unavailable: 'DNS down.', mutation: true }).message).toMatch(/^DNS down\. .*can't tell whether the change went through/);
    });

    it("describes a contract mismatch as a response the console didn't understand, not an outage", () => {
        expect(describeApiError(new InvalidResponseError()).message).toMatch(/didn't understand/);
    });
});

describe('normalizeDomain', () => {
    it.each([
        ['mail.example.com', 'mail.example.com'],
        [' Mail.Example.COM ', 'mail.example.com'],
        ['xn--bcher-kva.example', 'xn--bcher-kva.example'],
        ['a-b.c0', 'a-b.c0']
    ])('accepts %j as %j', (input, expected) => {
        expect(normalizeDomain(input)).toEqual({ domain: expected, error: null });
    });

    it.each(['', 'localhost', 'bücher.example', 'example.com.', 'a..b', '-a.com', 'a-.com', 'a_b.example', '192.0.2.1', 'a b.example', `${'a'.repeat(64)}.com`])(
        'rejects %j',
        input => {
            expect(normalizeDomain(input).domain).toBeNull();
        }
    );

    it('limits the total length to 253 characters', () => {
        const label = 'a'.repeat(63);
        expect(normalizeDomain([label, label, label, 'a'.repeat(61)].join('.')).domain).not.toBeNull();
        expect(normalizeDomain([label, label, label, 'a'.repeat(62)].join('.')).domain).toBeNull();
    });
});

describe('validateName', () => {
    it('accepts names the server accepts', () => {
        expect(validateName('Billing service')).toBeNull();
        expect(validateName('é'.repeat(64))).toBeNull();
    });

    it('rejects names the server rejects', () => {
        expect(validateName('')).not.toBeNull();
        expect(validateName(' padded')).not.toBeNull();
        expect(validateName('tab\there')).not.toBeNull();
        expect(validateName('del\u007f')).not.toBeNull();
        expect(validateName('é'.repeat(65))).not.toBeNull();
        expect(validateName('name\u0085')).not.toBeNull();
        expect(validateName('　name')).not.toBeNull();
    });

    it('matches the server where JS trim() differs', () => {
        expect(validateName('﻿name')).toBeNull();
    });
});
