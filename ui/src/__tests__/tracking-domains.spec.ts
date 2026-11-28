import { describe, expect, it } from 'vitest';

import { parseTrackingDomain, suggestTrackingName, trackingStatus, trackingSummaryLine, trackingView, validateTrackingName, type TrackingView } from '@/tracking-domains';

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
    it('is a short tag per state', () => {
        expect(trackingStatus(view({ state: 'active', httpsResult: 'ok' }), support)).toEqual({ label: 'In use', tone: 'success' });
        expect(trackingStatus(view(), support).label).toBe('Waiting for DNS record');
        expect(trackingStatus(view({ dnsResult: 'ok', httpsResult: 'certificate' }), support).label).toBe('Waiting for HTTPS');
        expect(trackingStatus(view({ state: 'failing', dnsResult: 'ok', httpsResult: 'certificate' }), support).label).toBe('Not in use');
        expect(trackingStatus(view({ state: 'removed' }), support).label).toBe('Removed');
    });

    it('is Not in use, whatever the state, when the installation does not use custom domains', () => {
        for (const state of ['active', 'pending', 'failing'] as const) {
            expect(trackingStatus(view({ state, httpsResult: 'ok' }), { available: false, cnameTarget: 'links.example.net' })).toEqual({ label: 'Not in use', tone: 'warning' });
        }
    });

    it('waits for HTTPS, not DNS, when the name answers some other way (a proxy)', () => {
        expect(trackingStatus(view({ dnsResult: 'wrong_target', httpsResult: 'certificate' }), support).label).toBe('Waiting for HTTPS');
        expect(trackingStatus(view({ dnsResult: 'lookup_failed', httpsResult: 'unreachable' }), support).label).toBe('Waiting for HTTPS');
        expect(trackingStatus(view({ dnsResult: 'wrong_target', httpsResult: 'unreachable' }), support).label).toBe('Waiting for DNS record');
        expect(trackingStatus(view({ dnsResult: 'missing', httpsResult: 'not_found' }), support).label).toBe('Waiting for DNS record');
    });
});

describe('trackingView', () => {
    it('shows an active domain as the record, a secure connection and the certificate expiry', () => {
        const v = trackingView(view({ state: 'active', dnsResult: 'wrong_target', httpsResult: 'ok', observedTarget: 'proxy.example.org', certificateExpiresAt: '2027-01-02T03:04:05Z' }), support);
        expect(v.tag.label).toBe('In use');
        // A proxied name never matches the record and is still in use: no fix, but marked.
        expect(v.record).toEqual({
            name: 'links.example.com',
            target: 'links.example.net',
            observed: { state: 'problem', text: 'proxy.example.org', hint: 'Fine only behind a proxy' },
            fix: null
        });
        expect(v.https).toEqual({ tag: { label: 'Secure', tone: 'success' }, line: null, expiresAt: '2027-01-02T03:04:05Z' });
        expect(v.note).toBeNull();
    });

    it('keeps an active domain in use after a check that could not finish, and says so briefly', () => {
        const v = trackingView(view({ state: 'active', httpsResult: 'unreachable' }), support);
        expect(v.tag.label).toBe('In use');
        expect(v.https?.line).toBe("Last check didn't finish. Thinpost will retry.");
        expect(v.record.fix).toBeNull();
    });

    it('asks to add the CNAME before anything has been checked, and when it is missing', () => {
        for (const patch of [{}, { dnsResult: 'missing', httpsResult: 'unreachable' }] as const) {
            const v = trackingView(view(patch), support);
            expect(v.record.observed).toMatchObject({ state: 'missing', text: 'links.example.net' });
            expect(v.record.fix).toEqual({ kind: 'add', host: 'links.example.com', value: 'links.example.net' });
        }
    });

    it('shows a record that points elsewhere as a problem and asks to update it', () => {
        const v = trackingView(view({ dnsResult: 'wrong_target', httpsResult: 'unreachable', observedTarget: 'cdn.example.org' }), support);
        expect(v.record.observed).toMatchObject({ state: 'problem', text: 'cdn.example.org' });
        expect(v.record.fix).toEqual({ kind: 'update', host: 'links.example.com', value: 'links.example.net' });
        expect(v.note).toContain('Cloudflare');
    });

    it('needs no DNS change once the record is right, and names the connection problem', () => {
        const v = trackingView(view({ dnsResult: 'ok', httpsResult: 'certificate' }), support);
        expect(v.record.fix).toBeNull();
        expect(v.https).toMatchObject({ tag: { label: 'No valid certificate', tone: 'warning' }, line: 'Your Thinpost administrator issues it.' });
    });

    it.each([
        ['unreachable', "Can't connect"],
        ['certificate', 'No valid certificate'],
        ['redirected', 'Redirects'],
        ['not_thinpost', 'Not this installation'],
        ['not_found', 'Not in DNS']
    ] as const)('tags the connection problem %s', (httpsResult, label) => {
        expect(trackingView(view({ dnsResult: 'ok', httpsResult }), support).https?.tag.label).toBe(label);
    });

    it('marks a failing domain as not in use', () => {
        expect(trackingView(view({ state: 'failing', dnsResult: 'ok', httpsResult: 'certificate' }), support).tag.label).toBe('Not in use');
    });

    it('says Not in use with one line when the installation does not use custom domains', () => {
        const v = trackingView(view({ state: 'active', httpsResult: 'ok' }), { available: false, cnameTarget: 'links.example.net' });
        expect(v.tag.label).toBe('Not in use');
        expect(v.note).toBe('Not turned on for this installation. Ask your Thinpost administrator.');
        expect(v.record.fix).toBeNull();
        expect(v.https).toBeNull();
    });

    it('shows a removed domain with one line and no fix', () => {
        const v = trackingView(view({ state: 'removed' }), support);
        expect(v.tag.label).toBe('Removed');
        expect(v.note).toBe('No longer used for new mail.');
        expect(v.record.fix).toBeNull();
        expect(v.https).toBeNull();
    });

    it('never invents a name to point at', () => {
        const v = trackingView(view(), { available: true, cnameTarget: null });
        expect(v.record).toEqual({ name: 'links.example.com', target: null, observed: null, fix: null });
        expect(v.note).toContain('not known yet');
    });
});

describe('trackingView edge cases', () => {
    const fix = { kind: 'update', host: 'links.example.com', value: 'links.example.net' };

    it.each(['certificate', 'not_thinpost', 'redirected', 'unreachable'] as const)('shows a wrong target as a problem with an Update line when HTTPS fails as %s', httpsResult => {
        const v = trackingView(view({ dnsResult: 'wrong_target', httpsResult, observedTarget: 'cdn.example.org' }), support);
        expect(v.record.observed).toMatchObject({ state: 'problem', text: 'cdn.example.org', hint: 'Points at the wrong name' });
        expect(v.record.fix).toEqual(fix);
        expect(v.note).toBe('Behind a proxy such as Cloudflare? Then only HTTPS must work.');
    });

    it('marks a wrong target on an active domain without a fix, and on a pending domain whose HTTPS works', () => {
        const active = trackingView(view({ state: 'active', dnsResult: 'wrong_target', httpsResult: 'certificate', observedTarget: 'cdn.example.org' }), support);
        expect(active.record.observed).toMatchObject({ state: 'problem', hint: 'Fine only behind a proxy' });
        expect(active.record.fix).toBeNull();
        const ok = trackingView(view({ dnsResult: 'wrong_target', httpsResult: 'ok', observedTarget: 'cdn.example.org' }), support);
        expect(ok.record.observed).toMatchObject({ hint: 'Fine only behind a proxy' });
        expect(ok.record.fix).toBeNull();
    });

    it('keeps the Add line, with a retry note, when the DNS lookup could not run', () => {
        const v = trackingView(view({ dnsResult: 'lookup_failed', httpsResult: 'unreachable' }), support);
        expect(v.record.observed).toBeNull();
        expect(v.record.fix).toEqual({ kind: 'add', host: 'links.example.com', value: 'links.example.net' });
        expect(v.note).toBe("Couldn't check DNS. Thinpost will retry.");
        expect(trackingView(view({ state: 'active', dnsResult: 'lookup_failed', httpsResult: 'ok' }), support).record.fix).toBeNull();
    });

    it('asks for nothing when the stale wrong target is already the wanted one', () => {
        const v = trackingView(view({ dnsResult: 'wrong_target', httpsResult: 'unreachable', observedTarget: 'links.example.net' }), support);
        expect(v.record.fix).toBeNull();
        expect(v.record.observed).toBeNull();
        expect(v.note).toBe('Matches now. Choose Check now.');
    });

    it('treats a CNAME that does not resolve as a problem to update, not to add', () => {
        const v = trackingView(view({ dnsResult: 'missing', httpsResult: 'not_found', observedTarget: 'gone.example.org' }), support);
        expect(v.record.observed).toMatchObject({ state: 'problem', text: 'gone.example.org', hint: "Points at a name that doesn't resolve" });
        expect(v.record.fix).toEqual(fix);
    });

    it('does not tell the administrator to finish setup while a DNS fix comes first', () => {
        expect(trackingView(view({ dnsResult: 'wrong_target', httpsResult: 'unreachable', observedTarget: 'cdn.example.org' }), support).https?.line).toBeNull();
        expect(trackingView(view({ dnsResult: 'missing', httpsResult: 'unreachable' }), support).https?.line).toBeNull();
        expect(trackingView(view({ dnsResult: 'ok', httpsResult: 'unreachable' }), support).https?.line).toBe('Your Thinpost administrator finishes setup.');
    });

    it('keeps the certificate expiry when custom tracking is switched off for the installation', () => {
        const off = { available: false, cnameTarget: 'links.example.net' };
        const v = trackingView(view({ state: 'active', httpsResult: 'ok', certificateExpiresAt: '2027-01-02T03:04:05Z' }), off);
        expect(v.https).toMatchObject({ tag: { label: 'Secure' }, expiresAt: '2027-01-02T03:04:05Z' });
        expect(trackingView(view({ state: 'active', httpsResult: 'ok' }), off).https).toBeNull();
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
    it('accepts a strict subdomain of the sending domain, normalized', () => {
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
