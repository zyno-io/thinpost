import { describe, expect, it } from 'vitest';

import { InvalidResponseError } from '@/api-errors';
import { dmarcView, parseSenderDnsCheck, senderDnsSummary, spfView } from '@/sender-dns';

type Check = ReturnType<typeof parseSenderDnsCheck>;

const DOMAIN = 'mail.example.com';
const check = (dmarc: object = {}, spf: object = {}): Check =>
    ({
        domain: DOMAIN,
        dmarc: { status: 'ok', org_domain: 'example.com', record_domain: DOMAIN, record: 'v=DMARC1; p=reject', policy: 'reject', aspf: 'r', ...dmarc },
        spf: { result: 'pass', envelope_domain: 'smtp.example.net', envelope_kind: 'standard', aligned: false, ...spf }
    }) as Check;

describe('parseSenderDnsCheck', () => {
    it('accepts the valid shape', () => {
        const value = check();
        expect(parseSenderDnsCheck(value, DOMAIN)).toBe(value);
        expect(() => parseSenderDnsCheck(check({ status: 'missing', record_domain: undefined, record: undefined, policy: undefined, aspf: undefined }, { result: 'no_sending_ip', envelope_domain: undefined, envelope_kind: undefined }), DOMAIN)).not.toThrow();
    });

    it.each([
        ['a body that is not an object', '<!doctype html>'],
        ['another domain', { ...check(), domain: 'other.example.com' }],
        ['an unknown DMARC status', check({ status: 'great' })],
        ['an unknown policy', check({ policy: 'block' })],
        ['an unknown alignment', check({ aspf: 'x' })],
        ['an unknown SPF result', check({}, { result: 'maybe' })],
        ['an unknown envelope kind', check({}, { envelope_kind: 'other' })],
        ['a missing envelope when SPF has a result', check({}, { envelope_domain: undefined })],
        ['a missing kind when SPF has a result', check({}, { envelope_kind: undefined })],
        ['an aligned flag that is not a boolean', check({}, { aligned: 'yes' })],
        ['a record_truncated that is not a boolean', check({ record_truncated: 'yes' })],
        ['an oversized record', check({ record: 'a'.repeat(1025) })],
        ['records that are not an array', check({ records: 'v=DMARC1' })],
        ['more than five records', check({ records: Array(6).fill('v=DMARC1; p=none') })],
        ['an oversized listed record', check({ records: ['a'.repeat(1025)] })],
        ['an oversized name', check({ org_domain: 'a'.repeat(254) })]
    ])('rejects %s', (_, value) => {
        expect(() => parseSenderDnsCheck(value, DOMAIN)).toThrow(InvalidResponseError);
    });
});

const dmarc = (record: string, extra: object = {}) => dmarcView(check({ status: 'monitoring', policy: 'none', record, ...extra }));
const states = (record: string) => dmarc(record).segments.map(p => `${p.state}:${p.text}`);

describe('dmarcView', () => {
    it('shows an enforced record with no advice, and the location it was read at', () => {
        const view = dmarcView(check());
        expect(view.tag).toEqual({ label: 'Enforced', tone: 'success' });
        expect(view.location).toBe('_dmarc.mail.example.com');
        expect(view.segments.map(p => p.state)).toEqual(['ok', 'ok']);
        expect(view.fix).toBeNull();
        expect(dmarcView(check({ record_domain: 'example.com' })).location).toBe('_dmarc.example.com');
    });

    it('shows monitoring with no advice to raise p', () => {
        const view = dmarcView(check({ status: 'monitoring', policy: 'none', record: 'v=DMARC1; p=none; rua=mailto:d@example.com; fo=1; x-unknown=1' }));
        expect(view.tag).toEqual({ label: 'Monitoring', tone: 'success' });
        expect(view.segments.every(p => p.state === 'ok')).toBe(true);
        expect(view.fix).toBeNull();
    });

    it('reads tag names and values case-insensitively', () => {
        const view = dmarc('v=DMARC1; P=REJECT; SP=None; ASPF=S; Adkim=R; PCT=100');
        expect(view.segments.every(p => p.state === 'ok')).toBe(true);
        expect(view.fix).toBeNull();
    });

    it('marks an invalid p, and corrects it to p=none', () => {
        const view = dmarc('v=DMARC1; p=block; rua=mailto:d@example.com');
        expect(states('v=DMARC1; p=block; rua=mailto:d@example.com')).toEqual(['ok:v=DMARC1', 'problem:p=block', 'ok:rua=mailto:d@example.com']);
        expect(view.segments[1]?.hint).toBe('p must be none, quarantine or reject');
        expect(view.fix).toEqual({ kind: 'update', value: 'v=DMARC1; p=none; rua=mailto:d@example.com' });
    });

    it('shows a missing p=none after v, also for a record with only rua', () => {
        const view = dmarc('v=DMARC1; rua=mailto:d@example.com');
        expect(states('v=DMARC1; rua=mailto:d@example.com')).toEqual(['ok:v=DMARC1', 'missing:p=none', 'ok:rua=mailto:d@example.com']);
        expect(view.segments[1]?.hint).toBeTruthy();
        expect(view.fix).toEqual({ kind: 'update', value: 'v=DMARC1; p=none; rua=mailto:d@example.com' });
        expect(dmarc('v=DMARC1').fix).toEqual({ kind: 'update', value: 'v=DMARC1; p=none' });
    });

    it('drops an invalid sp, aspf, adkim and pct, and keeps the rest verbatim', () => {
        const record = 'v=DMARC1; p=reject; sp=nope; aspf=x; adkim=; pct=101; rua=mailto:d@example.com; fo=1';
        const view = dmarc(record);
        expect(view.segments.map(p => p.state)).toEqual(['ok', 'ok', 'problem', 'problem', 'problem', 'problem', 'ok', 'ok']);
        expect(view.segments[2]?.hint).toBe('sp must be none, quarantine or reject');
        expect(view.segments[3]?.hint).toBe('aspf must be r or s');
        expect(view.segments[4]?.hint).toBe('adkim must be r or s');
        expect(view.segments[5]?.hint).toBe('pct must be a number from 0 to 100');
        expect(view.fix).toEqual({ kind: 'update', value: 'v=DMARC1; p=reject; rua=mailto:d@example.com; fo=1', warning: 'Applying this turns on p=reject.' });
        expect(dmarc('v=DMARC1; p=none; pct=abc').segments[2]?.state).toBe('problem');
        expect(dmarc('v=DMARC1; p=none; pct=0').fix).toBeNull();
    });

    it('warns when dropping an invalid sp turns on enforcement', () => {
        // The server: an invalid sp with rua counts as p=none, so monitoring.
        const withRua = dmarc('v=DMARC1; p=reject; sp=bogus; rua=mailto:a@b.c');
        expect(withRua.fix).toEqual({ kind: 'update', value: 'v=DMARC1; p=reject; rua=mailto:a@b.c', warning: 'Applying this turns on p=reject.' });
        // Without rua the server says invalid: receivers apply no DMARC.
        const noRua = dmarcView(check({ status: 'invalid', record: 'v=DMARC1; p=quarantine; sp=bogus', policy: undefined, aspf: undefined }));
        expect(noRua.fix).toEqual({ kind: 'update', value: 'v=DMARC1; p=quarantine', warning: 'Applying this turns on p=quarantine.' });
    });

    it('does not warn when the fix is not stricter than what applies now', () => {
        expect((dmarc('v=DMARC1; p=none; sp=bogus; rua=mailto:a@b.c').fix as { warning?: string }).warning).toBeUndefined();
        const enforced = dmarcView(check({ record: 'v=DMARC1; p=reject; sp=bogus' }));
        expect(enforced.fix).toEqual({ kind: 'update', value: 'v=DMARC1; p=reject' });
    });

    it('judges the fix by sp when the record is inherited', () => {
        const inherited = { record_domain: 'example.com' };
        // sp=none is valid and covers this domain, so reject for the parent does not apply to it.
        const view = dmarc('v=DMARC1; p=reject; sp=none; aspf=x; rua=mailto:a@b.c', inherited);
        expect((view.fix as { warning?: string }).warning).toBeUndefined();
        const own = dmarc('v=DMARC1; p=reject; sp=none; aspf=x; rua=mailto:a@b.c');
        expect(own.fix).toMatchObject({ warning: 'Applying this turns on p=reject.' });
        const dropped = dmarc('v=DMARC1; p=reject; sp=bogus; rua=mailto:a@b.c', inherited);
        expect(dropped.fix).toMatchObject({ warning: 'Applying this turns on p=reject.' });
    });

    it('shows a cut record as far as it goes, with no missing parts and no fix', () => {
        const record = 'v=DMARC1; rua=mailto:a@b.c; rua=mailto:c@d.e; adkim=s; p=rej';
        const view = dmarcView(check({ record, record_truncated: true }));
        expect(view.truncated).toBe(true);
        expect(view.segments.every(p => p.state === 'ok')).toBe(true);
        expect(view.segments.some(p => p.state === 'missing')).toBe(false);
        expect(view.fix).toBeNull();
        expect(dmarcView(check({ record: 'v=DMARC1; p=bogus; rua=x', record_truncated: true })).segments[1]?.state).toBe('problem');
        expect(dmarcView(check({ record: 'v=DMARC1; p=reject' })).truncated).toBeUndefined();
    });

    it('drops a malformed tag', () => {
        const view = dmarc('v=DMARC1; p=none; oops; rua=mailto:d@example.com;');
        expect(view.segments.map(p => `${p.state}:${p.text}`)).toEqual(['ok:v=DMARC1', 'ok:p=none', 'problem:oops', 'ok:rua=mailto:d@example.com']);
        expect(view.segments[2]?.hint).toBe('Not a tag=value pair');
        expect(view.fix).toEqual({ kind: 'update', value: 'v=DMARC1; p=none; rua=mailto:d@example.com' });
    });

    it('handles the invalid example from the console screenshots', () => {
        const view = dmarcView(check({ status: 'invalid', record: 'v=DMARC1; p=block; aspf=x; rua=mailto:dmarc@example.com', policy: undefined, aspf: undefined }));
        expect(view.tag).toEqual({ label: 'Not valid', tone: 'warning' });
        expect(view.fix).toEqual({ kind: 'update', value: 'v=DMARC1; p=none; rua=mailto:dmarc@example.com' });
    });

    it('tells what to add at the organizational domain when there is no record', () => {
        const view = dmarcView(check({ status: 'missing', record: undefined, record_domain: undefined, policy: undefined, aspf: undefined }));
        expect(view.tag).toEqual({ label: 'Missing', tone: 'warning' });
        expect(view.segments).toEqual([]);
        expect(view.location).toBeUndefined();
        expect(view.fix).toEqual({ kind: 'add', name: 'TXT _dmarc.example.com', value: 'v=DMARC1; p=none' });
    });

    it('lists duplicate records and says to keep one', () => {
        const records = ['v=DMARC1; p=none', 'v=DMARC1; p=reject'];
        const view = dmarcView(check({ status: 'multiple', records, record: undefined }));
        expect(view.tag).toEqual({ label: 'Duplicate records', tone: 'warning' });
        expect(view.location).toBe('_dmarc.mail.example.com');
        expect(view.records).toEqual(records);
        expect(view.segments).toEqual([]);
        expect(view.fix).toEqual({ kind: 'keep-one', text: 'Keep one; delete the others.' });
    });

    it('shows only the tag when the lookup was unavailable', () => {
        expect(dmarcView(check({ status: 'unavailable', record: undefined }))).toEqual({ tag: { label: "Couldn't check", tone: '' }, segments: [], records: [], fix: null });
    });
});

describe('spfView', () => {
    it('shows a pass that is aligned, at a bounce domain', () => {
        const view = spfView(check({}, { envelope_domain: 'bounces.example.com', envelope_kind: 'bounce_domain', aligned: true }));
        expect(view).toEqual({ result: { label: 'Passes', tone: 'success' }, alignment: { label: 'Aligned', tone: 'success' }, envelope: 'bounces.example.com', standard: false, note: null });
    });

    it('shows a pass that is not aligned as neutral, with the reason', () => {
        const view = spfView(check());
        expect(view.alignment).toEqual({ label: 'Not aligned', tone: '', title: 'DKIM aligns instead. A bounce domain aligns SPF too.' });
        expect(view.standard).toBe(true);
        expect(view.envelope).toBe('smtp.example.net');
        expect(view.note).toBeNull();
    });

    it('explains strict SPF in the tooltip', () => {
        expect(spfView(check({ aspf: 's' })).alignment?.title).toBe('Your DMARC record asks for strict SPF (aspf=s). DKIM aligns instead.');
    });

    it.each(['fail', 'softfail', 'neutral', 'none', 'permerror'])('shows %s as Fails with who sets it', result => {
        const view = spfView(check({}, { result }));
        expect(view.result).toEqual({ label: 'Fails', tone: 'warning' });
        expect(view.alignment).toBeNull();
        expect(view.note).toBe('Set by your Thinpost administrator, not on mail.example.com.');
    });

    it("doesn't warn when it couldn't check", () => {
        const temp = spfView(check({}, { result: 'temperror' }));
        expect(temp.result).toEqual({ label: "Couldn't check", tone: '' });
        expect(temp.note).toBeNull();
        const none = spfView(check({}, { result: 'no_sending_ip', envelope_domain: undefined, envelope_kind: undefined }));
        expect(none.result).toEqual({ label: "Couldn't check", tone: '' });
        expect(none.note).toBe('No sending IP reported yet.');
        expect(none.envelope).toBe('');
        expect(none.standard).toBe(false);
    });
});

describe('senderDnsSummary', () => {
    const noIp = { result: 'no_sending_ip', envelope_domain: undefined, envelope_kind: undefined };

    it('needs attention for a missing, invalid or duplicate DMARC record, a fix, or an SPF failure', () => {
        const attention = { label: 'Needs attention', tone: 'warning' };
        expect(senderDnsSummary(check({ status: 'missing' }, { result: 'temperror' }))).toEqual(attention);
        expect(senderDnsSummary(check({ status: 'invalid' }))).toEqual(attention);
        expect(senderDnsSummary(check({ status: 'multiple' }))).toEqual(attention);
        expect(senderDnsSummary(check({ status: 'ok', record: 'v=DMARC1; p=reject; pct=x' }))).toEqual(attention);
        expect(senderDnsSummary(check({ status: 'unavailable' }, { result: 'fail' }))).toEqual(attention);
        expect(senderDnsSummary(check({}, { result: 'softfail' }))).toEqual(attention);
    });

    it("says it couldn't check everything", () => {
        const partial = { label: "Couldn't check everything", tone: '' };
        expect(senderDnsSummary(check({ status: 'unavailable' }))).toEqual(partial);
        expect(senderDnsSummary(check({}, noIp))).toEqual(partial);
        expect(senderDnsSummary(check({}, { result: 'temperror' }))).toEqual(partial);
    });

    it('is all good for monitoring and an SPF pass that is not aligned', () => {
        expect(senderDnsSummary(check({ status: 'monitoring', policy: 'none', record: 'v=DMARC1; p=none' }, { aligned: false }))).toEqual({ label: 'All good', tone: 'success' });
        expect(senderDnsSummary(check())).toEqual({ label: 'All good', tone: 'success' });
    });
});
