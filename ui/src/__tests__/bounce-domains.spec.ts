import { describe, expect, it } from 'vitest';

import { bounceStatus, parseBounceDomain, parseCnameTarget, validateBounceName, type BounceView } from '@/bounce-domains';

const view = (patch: Partial<BounceView> = {}): BounceView => ({
    name: 'bounces.example.com',
    state: 'pending',
    checking: false,
    result: null,
    observedTarget: null,
    checkedAt: null,
    verifiedAt: null,
    ...patch
});
const target = { status: 'ok', target: 'smtp.example.net' } as const;

describe('parseBounceDomain', () => {
    it('reads a bounce domain', () => {
        expect(parseBounceDomain({ name: 'b.example.com', state: 'active', checking: true, result: 'ok', observed_target: 'smtp.example.net', checked_at: '2026-11-21T12:00:00Z', created_at: 'x' })).toEqual({
            name: 'b.example.com',
            state: 'active',
            checking: true,
            result: 'ok',
            observedTarget: 'smtp.example.net',
            checkedAt: '2026-11-21T12:00:00Z',
            verifiedAt: null
        });
    });

    it('is tolerant of an unknown result and strict about the state', () => {
        expect(parseBounceDomain({ name: 'b.example.com', state: 'pending', result: 'from_the_future' })?.result).toBeNull();
        for (const bad of [null, undefined, 'x', [], {}, { name: 'b.example.com' }, { name: 'b.example.com', state: 'verifying' }, { state: 'active' }, { name: '', state: 'active' }]) {
            expect(parseBounceDomain(bad)).toBeNull();
        }
    });
});

describe('parseCnameTarget', () => {
    it('shows the name only when the API confirmed it', () => {
        expect(parseCnameTarget({ cname_target: 'smtp.example.net', cname_target_status: 'ok' })).toEqual(target);
        expect(parseCnameTarget({ cname_target_status: 'dns_unconfirmed' })).toEqual({ status: 'dns_unconfirmed', target: null });
        expect(parseCnameTarget({ cname_target_status: 'not_reported' })).toEqual({ status: 'not_reported', target: null });
        // A status that says ok without a name is not an answer, and neither is an absent or unknown status.
        expect(parseCnameTarget({ cname_target_status: 'ok' })).toEqual({ status: 'not_reported', target: null });
        expect(parseCnameTarget({})).toEqual({ status: 'not_reported', target: null });
        expect(parseCnameTarget({ cname_target: 'smtp.example.net', cname_target_status: 'bogus' }).target).toBeNull();
    });
});

describe('bounceStatus', () => {
    it('says what an active domain does and how DMARC sees it', () => {
        const s = bounceStatus(view({ state: 'active', result: 'ok' }), 'example.com', target);
        expect(s).toMatchObject({ label: 'In use', tone: 'success' });
        expect(s.message).toContain('Bounces for mail from example.com come back to bounces.example.com, so SPF passes for your domain too');
        expect(s.message).toContain('aspf=s');
        expect(s.message).not.toContain("couldn't finish");
    });

    it('keeps an active domain in use after a check that could not finish, and says so', () => {
        const s = bounceStatus(view({ state: 'active', result: 'lookup_failed' }), 'example.com', target);
        expect(s.label).toBe('In use');
        expect(s.message).toContain("couldn't finish");
    });

    it('asks for the CNAME, DNS only, while waiting', () => {
        const s = bounceStatus(view(), 'example.com', target);
        expect(s.label).toBe('Waiting for DNS record');
        expect(s.message).toContain('CNAME record for bounces.example.com pointing at smtp.example.net (DNS only, not proxied)');
        expect(s.message).toContain("standard bounce address");
    });

    it.each([
        ['missing', "can't find bounces.example.com in DNS"],
        ['not_cname', "isn't a CNAME"],
        ['other_ip', 'This app now sends from a different IP. Point the CNAME at smtp.example.net.'],
        ['wrong_target', 'points at cdn.other.example, not smtp.example.net']
    ] as const)('explains a failing domain with result %s', (result, expected) => {
        const s = bounceStatus(view({ state: 'failing', result, observedTarget: 'cdn.other.example' }), 'example.com', target);
        expect(s).toMatchObject({ label: 'Not in use', tone: 'warning' });
        expect(s.message).toContain(expected);
        expect(s.message).toContain('Nothing is lost.');
    });

    it('tells a proxied record to be turned off', () => {
        expect(bounceStatus(view({ state: 'failing', result: 'not_cname' }), 'example.com', target).message).toContain('orange cloud');
        expect(bounceStatus(view({ state: 'failing', result: 'not_cname' }), 'example.com', target).message).toContain('DNS only, not proxied');
    });

    it('does not tell anyone to check DNS when Thinpost could not', () => {
        const s = bounceStatus(view({ state: 'failing', result: 'lookup_failed' }), 'example.com', target);
        expect(s.message).toContain("couldn't check DNS just now");
    });

    it('never invents a name to point at', () => {
        const unconfirmed = bounceStatus(view({ state: 'failing', result: 'missing' }), 'example.com', { status: 'dns_unconfirmed', target: null });
        expect(unconfirmed.message).toContain("can't confirm the name to point at");
        const unreported = bounceStatus(view(), 'example.com', { status: 'not_reported', target: null });
        expect(unreported.message).toContain("hasn't reported in");
        expect(unreported.message).not.toContain('undefined');
    });
});

describe('validateBounceName', () => {
    it('accepts a strict subdomain and normalizes it', () => {
        expect(validateBounceName('  Bounces.Example.com ', 'example.com')).toEqual({ name: 'bounces.example.com', error: null });
        expect(validateBounceName('a.b.example.com', 'example.com').name).toBe('a.b.example.com');
    });

    it.each([
        ['', 'Enter a domain.'],
        ['example.com', 'Use a subdomain of example.com, such as bounces.example.com.'],
        ['bounces.not' + 'example.com', 'Use a subdomain of example.com'],
        ['bounces.example.org', 'Use a subdomain of example.com'],
        ['.example.com', 'Remove the empty part'],
        ['bücher.example.com', 'xn-- form'],
        ['bounces.example.com.', 'trailing dot']
    ])('refuses %j', (input, message) => {
        const result = validateBounceName(input, 'example.com');
        expect(result.name).toBeNull();
        expect(result.error).toContain(message);
    });

    it('refuses a name longer than a Return-Path can hold', () => {
        const long = `${'a'.repeat(60)}.${'b'.repeat(60)}.${'c'.repeat(60)}.${'d'.repeat(40)}.example.com`;
        expect(validateBounceName(long, 'example.com').error).toContain('at most 219');
    });
});
