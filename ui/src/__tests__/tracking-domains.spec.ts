import { describe, expect, it } from 'vitest';

import { parseTrackingDomain, suggestTrackingName, trackingStatus, trackingSummaryLine, validateTrackingName, type TrackingView } from '@/tracking-domains';

const view = (patch: Partial<TrackingView> = {}): TrackingView => ({
    name: 'links.example.com',
    state: 'pending',
    checking: false,
    dnsResult: null,
    httpsResult: null,
    observedTarget: null,
    certificateExpiresAt: null,
    checkedAt: null,
    verifiedAt: null,
    ...patch
});
const support = { available: true, cnameTarget: 'links.example.net' };

describe('parseTrackingDomain', () => {
    it('reads a tracking domain', () => {
        expect(
            parseTrackingDomain({
                name: 'links.example.com',
                state: 'active',
                checking: true,
                dns_result: 'wrong_target',
                https_result: 'ok',
                observed_target: 'cdn.example.org',
                certificate_expires_at: '2027-01-02T03:04:05Z',
                checked_at: '2026-11-21T12:00:00Z',
                created_at: 'x'
            })
        ).toEqual({
            name: 'links.example.com',
            state: 'active',
            checking: true,
            dnsResult: 'wrong_target',
            httpsResult: 'ok',
            observedTarget: 'cdn.example.org',
            certificateExpiresAt: '2027-01-02T03:04:05Z',
            checkedAt: '2026-11-21T12:00:00Z',
            verifiedAt: null
        });
    });

    it('is tolerant of an unknown result and strict about the state', () => {
        expect(parseTrackingDomain({ name: 'l.example.com', state: 'pending', https_result: 'from_the_future', dns_result: 'host_changed' })).toMatchObject({ httpsResult: null, dnsResult: null });
        for (const bad of [null, undefined, 'x', [], {}, { name: 'l.example.com' }, { name: 'l.example.com', state: 'verifying' }, { state: 'active' }, { name: '', state: 'active' }]) {
            expect(parseTrackingDomain(bad)).toBeNull();
        }
    });
});

describe('trackingStatus', () => {
    it('says what an active domain covers, and that unsubscribe stays standard', () => {
        const s = trackingStatus(view({ state: 'active', httpsResult: 'ok' }), 'example.com', support);
        expect(s).toMatchObject({ label: 'In use', tone: 'success' });
        expect(s.message).toContain('Tracked links and the open-tracking image in mail from example.com use links.example.com');
        expect(s.message).toContain("One-click unsubscribe links keep using Thinpost's standard address");
        expect(s.message).not.toContain("couldn't finish");
    });

    it('keeps an active domain in use after a check that could not finish, and says so', () => {
        const s = trackingStatus(view({ state: 'active', httpsResult: 'unreachable' }), 'example.com', support);
        expect(s.label).toBe('In use');
        expect(s.message).toContain("couldn't finish");
    });

    it('does not treat a DNS miss as a problem for an active domain (a proxied name)', () => {
        const s = trackingStatus(view({ state: 'active', dnsResult: 'wrong_target', httpsResult: 'ok' }), 'example.com', support);
        expect(s).toMatchObject({ label: 'In use', tone: 'success' });
        expect(s.message).not.toContain('CNAME');
    });

    it('asks for the DNS record before anything has been checked', () => {
        const s = trackingStatus(view(), 'example.com', support);
        expect(s.label).toBe('Waiting for DNS record');
        expect(s.message).toContain('Add a CNAME record for links.example.com pointing at links.example.net');
        expect(trackingStatus(view(), 'example.com', { available: true, cnameTarget: null }).message).toContain('the name shown below');
    });

    it('waits for a secure connection once the DNS record is right', () => {
        const s = trackingStatus(view({ dnsResult: 'ok', httpsResult: 'certificate' }), 'example.com', support);
        expect(s).toMatchObject({ label: 'Waiting for a secure connection', tone: 'warning' });
        expect(s.message).toContain('Waiting for a secure connection. Your DNS record is right; your Thinpost administrator still needs to finish setting up this address.');
        expect(s.message).toContain("doesn't have a valid security certificate yet");
        expect(s.message).toContain('Links will start using it on their own once it works.');
    });

    it.each([
        ['unreachable', "couldn't reach links.example.com"],
        ['certificate', 'valid security certificate'],
        ['redirected', 'sends visitors somewhere else'],
        ['not_thinpost', 'not from this Thinpost installation']
    ] as const)('names the connection problem for %s', (httpsResult, expected) => {
        expect(trackingStatus(view({ dnsResult: 'ok', httpsResult }), 'example.com', support).message).toContain(expected);
    });

    it('explains a missing record and a record that points elsewhere', () => {
        const missing = trackingStatus(view({ dnsResult: 'missing', httpsResult: 'unreachable' }), 'example.com', support);
        expect(missing.label).toBe('Waiting for DNS record');
        expect(missing.message).toContain("can't find links.example.com in DNS");
        const elsewhere = trackingStatus(view({ dnsResult: 'wrong_target', httpsResult: 'unreachable', observedTarget: 'cdn.example.org' }), 'example.com', support);
        expect(elsewhere.message).toContain('points at cdn.example.org, not links.example.net');
        expect(elsewhere.message).toContain('Cloudflare');
    });

    it('marks a failing domain as not in use and says new links use the standard address', () => {
        const s = trackingStatus(view({ state: 'failing', dnsResult: 'ok', httpsResult: 'certificate' }), 'example.com', support);
        expect(s.label).toBe('Not in use');
        expect(s.message).toContain('Not in use.');
        expect(s.message).toContain("New links use Thinpost's standard tracking address in the meantime. Nothing is lost.");
    });

    it('says not in use, whatever the state, when the installation does not use custom domains', () => {
        for (const state of ['active', 'pending', 'failing'] as const) {
            const s = trackingStatus(view({ state, httpsResult: 'ok' }), 'example.com', { available: false, cnameTarget: 'links.example.net' });
            expect(s).toMatchObject({ label: 'Not in use', tone: 'warning' });
            expect(s.message).toContain("this installation doesn't use your own tracking address right now");
        }
    });

    it('says the name is gone from DNS for not_found', () => {
        const s = trackingStatus(view({ dnsResult: 'missing', httpsResult: 'not_found' }), 'example.com', support);
        expect(s.message).toContain("can't find links.example.com in DNS");
        expect(parseTrackingDomain({ name: 'l.example.com', state: 'failing', https_result: 'not_found' })?.httpsResult).toBe('not_found');
    });

    it('says it will look again when DNS could not be checked', () => {
        const s = trackingStatus(view({ dnsResult: 'lookup_failed', httpsResult: 'unreachable' }), 'example.com', support);
        expect(s.message).toContain('It will try again on its own');
    });
});

describe('summary line', () => {
    it('is one short phrase per state', () => {
        expect(trackingSummaryLine(view({ state: 'active' }))).toBe('In use');
        expect(trackingSummaryLine(view({ state: 'failing' }))).toBe('Not in use');
        expect(trackingSummaryLine(view())).toBe('Waiting for setup');
    });
});

describe('validateTrackingName', () => {
    it('accepts a strict subdomain of the sending domain, normalised', () => {
        expect(validateTrackingName(' Links.Example.COM ', 'example.com')).toEqual({ name: 'links.example.com', error: null });
        expect(validateTrackingName('a.b.example.com', 'example.com').name).toBe('a.b.example.com');
        expect(suggestTrackingName('example.com')).toBe('links.example.com');
    });

    it('refuses the sending domain itself, other domains and lookalikes', () => {
        for (const bad of ['example.com', 'links.example.org', 'links' + 'example.com', 'links.not' + 'example.com']) {
            expect(validateTrackingName(bad, 'example.com').error).toBe('Use a subdomain of example.com, such as links.example.com.');
        }
    });

    it('refuses what is not a domain, and names over 253 characters', () => {
        for (const bad of ['', 'not a domain', '10.0.0.1', 'links.example.com.']) {
            expect(validateTrackingName(bad, 'example.com').name).toBeNull();
        }
        expect(validateTrackingName(`${'a'.repeat(60)}.${'b'.repeat(60)}.${'c'.repeat(60)}.${'d'.repeat(60)}.example.com`, 'example.com').error).toBe('Use at most 253 characters.');
    });
});
