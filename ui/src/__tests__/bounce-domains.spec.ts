import { describe, expect, it } from 'vitest';

import { bounceStatus, bounceView, inheritedFrom, parseBounceDomain, parseCnameTarget, validateBounceName, type BounceView } from '@/bounce-domains';

const view = (patch: Partial<BounceView> = {}): BounceView => ({
    name: 'bounces.example.com',
    state: 'pending',
    checking: false,
    result: null,
    observedTarget: null,
    checkedAt: null,
    lastOkAt: null,
    verifiedAt: null,
    ...patch
});
const target = { status: 'ok', target: 'smtp.example.net' } as const;

describe('parseBounceDomain', () => {
    it('reads a bounce domain', () => {
        expect(parseBounceDomain({ name: 'b.example.com', state: 'active', checking: true, result: 'ok', observed_target: 'smtp.example.net', checked_at: '2026-11-21T12:00:00Z', last_ok_at: '2026-11-21T11:00:00Z', created_at: 'x' })).toEqual({
            name: 'b.example.com',
            state: 'active',
            checking: true,
            result: 'ok',
            observedTarget: 'smtp.example.net',
            checkedAt: '2026-11-21T12:00:00Z',
            lastOkAt: '2026-11-21T11:00:00Z',
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
    it('is a short tag per state', () => {
        expect(bounceStatus(view({ state: 'active', result: 'ok' }), target)).toEqual({ label: 'In use', tone: 'success' });
        expect(bounceStatus(view(), target)).toEqual({ label: 'Waiting for DNS record', tone: 'warning' });
        expect(bounceStatus(view({ state: 'failing', result: 'lookup_failed' }), target).label).toBe('Not in use');
        expect(bounceStatus(view({ result: 'missing' }), target).label).toBe('Not in use');
        expect(bounceStatus(view({ state: 'failing' }), { status: 'not_reported', target: null }).label).toBe('Not in use');
        expect(bounceStatus(view(), { status: 'not_reported', target: null }).label).toBe('Waiting for DNS record');
    });
});

describe('bounceView', () => {
    it('shows an active domain as the record alone', () => {
        const v = bounceView(view({ state: 'active', result: 'ok' }), target);
        expect(v.tag.label).toBe('In use');
        expect(v.record).toEqual({ name: 'bounces.example.com', target: 'smtp.example.net', observed: null, fix: null });
        expect(v.note).toBeNull();
    });

    it('keeps an active domain in use after a check that could not finish, and says so briefly', () => {
        const v = bounceView(view({ state: 'active', result: 'lookup_failed' }), target);
        expect(v.tag.label).toBe('In use');
        expect(v.note).toBe("Last check didn't finish. Thinpost will retry.");
        expect(v.record.fix).toBeNull();
    });

    it('asks to add the CNAME while waiting, and when it is missing', () => {
        for (const result of [null, 'missing'] as const) {
            const v = bounceView(view({ result }), target);
            expect(v.record.observed).toMatchObject({ state: 'missing', text: 'smtp.example.net' });
            expect(v.record.fix).toEqual({ kind: 'add', host: 'bounces.example.com', value: 'smtp.example.net' });
            expect(v.note).toBe('DNS only, not proxied.');
        }
    });

    it.each([
        ['not_cname', 'not a CNAME', 'Turn off any proxy (orange cloud).'],
        ['other_ip', 'cdn.other.example', 'This app now sends from a different IP.'],
        ['wrong_target', 'cdn.other.example', 'DNS only, not proxied.']
    ] as const)('shows what was found and asks to update for result %s', (result, observed, note) => {
        const v = bounceView(view({ state: 'failing', result, observedTarget: 'cdn.other.example' }), target);
        expect(v.tag).toEqual({ label: 'Not in use', tone: 'warning' });
        expect(v.record.observed).toMatchObject({ state: 'problem', text: observed });
        expect(v.record.fix).toEqual({ kind: 'update', host: 'bounces.example.com', value: 'smtp.example.net' });
        expect(v.note).toBe(note);
    });

    it('names another name when none was observed', () => {
        expect(bounceView(view({ state: 'failing', result: 'wrong_target' }), target).record.observed?.text).toBe('another name');
    });

    it.each(['lookup_failed', 'no_sending_ip'] as const)('keeps the Add line, with a retry note, when Thinpost could not check (%s)', result => {
        const v = bounceView(view({ state: 'pending', result }), target);
        expect(v.record.observed).toBeNull();
        expect(v.record.fix).toEqual({ kind: 'add', host: 'bounces.example.com', value: 'smtp.example.net' });
        expect(v.note).toBe("Couldn't check DNS. Thinpost will retry.");
        expect(bounceView(view({ state: 'failing', result }), target).record.fix?.kind).toBe('add');
    });

    it('does not claim the record is absent when a check could not run but a name was seen', () => {
        const v = bounceView(view({ state: 'failing', result: 'lookup_failed', observedTarget: 'cdn.other.example' }), target);
        expect(v.record.fix).toEqual({ kind: 'update', host: 'bounces.example.com', value: 'smtp.example.net' });
    });

    it('never invents a name to point at', () => {
        const unconfirmed = bounceView(view({ state: 'failing', result: 'missing' }), { status: 'dns_unconfirmed', target: null });
        expect(unconfirmed.note).toContain("Can't confirm the name to point at");
        expect(unconfirmed.record).toEqual({ name: 'bounces.example.com', target: null, observed: null, fix: null });
        const unreported = bounceView(view(), { status: 'not_reported', target: null });
        expect(unreported.note).toContain('not known yet');
        expect(JSON.stringify(unreported)).not.toContain('undefined');
    });
});

describe('bounceView against what mail is using', () => {
    const NOW = Date.parse('2026-11-21T12:00:00Z');
    const fresh = '2026-11-21T10:00:00Z';

    it('is in use while the passing check is fresh and the record matches', () => {
        const v = bounceView(view({ state: 'active', result: 'ok', observedTarget: 'smtp.example.net', lastOkAt: fresh }), target, NOW);
        expect(v.tag.label).toBe('In use');
        expect(v.record.fix).toBeNull();
    });

    it('is Not in use, with an Update line, when active but the record points at another name now', () => {
        const row = view({ state: 'active', result: 'ok', observedTarget: 'smtp.old.example.net', lastOkAt: fresh });
        const v = bounceView(row, target, NOW);
        expect(v.tag).toEqual({ label: 'Not in use', tone: 'warning' });
        expect(bounceStatus(row, target, NOW).label).toBe('Not in use');
        expect(v.record.observed).toMatchObject({ state: 'problem', text: 'smtp.old.example.net' });
        expect(v.record.fix).toEqual({ kind: 'update', host: 'bounces.example.com', value: 'smtp.example.net' });
    });

    it('is Not in use, with a retry note, when the last pass is 3 hours old or older', () => {
        const v = bounceView(view({ state: 'active', result: 'ok', observedTarget: 'smtp.example.net', lastOkAt: '2026-11-21T09:00:00Z' }), target, NOW);
        expect(v.tag.label).toBe('Not in use');
        expect(v.note).toBe("Last check didn't finish. Thinpost will retry.");
        expect(v.record.fix).toBeNull();
        expect(bounceView(view({ state: 'active', result: 'ok', lastOkAt: '2026-11-21T09:00:01Z' }), target, NOW).tag.label).toBe('In use');
    });

    it('does not apply freshness when the API gave no last pass', () => {
        expect(bounceView(view({ state: 'active', result: 'ok' }), target, NOW).tag.label).toBe('In use');
    });

    it.each(['other_ip', 'wrong_target'] as const)('asks for nothing when the stale result %s names what is wanted now', result => {
        const v = bounceView(view({ state: 'failing', result, observedTarget: 'smtp.example.net' }), target, NOW);
        expect(v.record.fix).toBeNull();
        expect(v.record.observed).toBeNull();
        expect(v.note).toBe('Matches now. Choose Check now.');
    });
});

describe('inheritedFrom', () => {
    const domains = [{ name: 'example.com', own: 'bounces.example.com' }, { name: 'a.example.com', own: null }, { name: 'b.a.example.com', own: 'b.bounces.example.com' }];
    const pick = (d: { own: string | null }) => d.own;

    it('finds the nearest parent in the list with one in use', () => {
        expect(inheritedFrom('c.a.example.com', domains, pick)).toEqual({ from: 'example.com', name: 'bounces.example.com' });
        expect(inheritedFrom('x.b.a.example.com', domains, pick)).toEqual({ from: 'b.a.example.com', name: 'b.bounces.example.com' });
    });

    it('is null for a top domain, an unrelated name, or a lookalike', () => {
        expect(inheritedFrom('example.com', domains, pick)).toBeNull();
        expect(inheritedFrom('mail.example.org', domains, pick)).toBeNull();
        expect(inheritedFrom('mail' + 'example.com', domains, pick)).toBeNull();
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
