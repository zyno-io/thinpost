import { describe, expect, it } from 'vitest';

import { InvalidResponseError } from '@/api-errors';
import { dmarcLine, parseSenderDnsCheck, senderDnsSummary, spfLine } from '@/sender-dns';

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
        ['an oversized record', check({ record: 'a'.repeat(1025) })],
        ['an oversized name', check({ org_domain: 'a'.repeat(254) })]
    ])('rejects %s', (_, value) => {
        expect(() => parseSenderDnsCheck(value, DOMAIN)).toThrow(InvalidResponseError);
    });
});

describe('dmarcLine', () => {
    it('describes an enforced policy, and notes an inherited record', () => {
        const own = dmarcLine(check());
        expect(own).toMatchObject({ label: 'Enforced', tone: 'success' });
        expect(own.message).toContain('asked to reject mail from mail.example.com');
        expect(own.message).not.toContain('covers its subdomains');
        const quarantine = dmarcLine(check({ policy: 'quarantine', record_domain: 'example.com' }));
        expect(quarantine.message).toContain('treat as spam');
        expect(quarantine.message).toContain('_dmarc.example.com, which covers its subdomains');
    });

    it('describes monitoring as fine, with the next step', () => {
        expect(dmarcLine(check({ status: 'monitoring', policy: 'none' }))).toMatchObject({ label: 'Monitoring only', tone: 'success' });
        expect(dmarcLine(check({ status: 'monitoring', policy: 'none' })).message).toContain('p=quarantine or p=reject');
    });

    it('tells what to add when there is no record', () => {
        const line = dmarcLine(check({ status: 'missing' }));
        expect(line).toMatchObject({ label: 'Not found', tone: 'warning' });
        expect(line.message).toContain('for mail.example.com or example.com');
        expect(line.message).toContain('_dmarc.example.com');
        expect(dmarcLine({ ...check({ status: 'missing', org_domain: DOMAIN }) }).message).not.toContain(' or ');
    });

    it('flags multiple and invalid records, and an unavailable lookup', () => {
        expect(dmarcLine(check({ status: 'multiple' }))).toMatchObject({ label: 'More than one', tone: 'warning' });
        expect(dmarcLine(check({ status: 'multiple' })).message).toContain('_dmarc.mail.example.com');
        expect(dmarcLine(check({ status: 'invalid' }))).toMatchObject({ label: 'Not valid', tone: 'warning' });
        expect(dmarcLine(check({ status: 'unavailable' }))).toMatchObject({ label: "Couldn't check", tone: '' });
    });
});

describe('spfLine', () => {
    it('describes a passing, aligned bounce domain', () => {
        const line = spfLine(check({}, { envelope_domain: 'bounces.example.com', envelope_kind: 'bounce_domain', aligned: true }));
        expect(line).toMatchObject({ label: 'Passes', tone: 'success' });
        expect(line.message).toBe('SPF passes for bounces.example.com, the bounce domain in use, and lines up with DMARC.');
    });

    it('explains a pass that does not line up as fine', () => {
        expect(spfLine(check({ aspf: 's' }, { envelope_domain: 'bounces.example.com', envelope_kind: 'bounce_domain' })).message).toContain('strict SPF (aspf=s)');
        const standard = spfLine(check());
        expect(standard.tone).toBe('success');
        expect(standard.message).toContain("Thinpost's standard bounce address");
        expect(standard.message).toContain('add a bounce domain below');
    });

    it.each([
        ['none', "there's no SPF record"],
        ['fail', "doesn't list the IP this app sends from"],
        ['softfail', "doesn't list the IP this app sends from"],
        ['neutral', "doesn't say whether the IP this app sends from may send"],
        ['permerror', 'needs more than 10 DNS lookups']
    ])('warns about %s', (result, reason) => {
        const line = spfLine(check({}, { result }));
        expect(line).toMatchObject({ label: "Doesn't pass", tone: 'warning' });
        expect(line.message).toContain(reason);
        expect(line.message).toContain("You don't need to change mail.example.com's own SPF record");
    });

    it("doesn't warn when it couldn't check", () => {
        expect(spfLine(check({}, { result: 'temperror' }))).toMatchObject({ label: "Couldn't check", tone: '' });
        const none = spfLine(check({}, { result: 'no_sending_ip', envelope_domain: undefined, envelope_kind: undefined }));
        expect(none).toMatchObject({ label: "Couldn't check", tone: '' });
        expect(none.message).toContain("hasn't reported in");
    });
});

describe('senderDnsSummary', () => {
    it('lets a warning beat what could not be checked, which beats all good', () => {
        expect(senderDnsSummary(check({ status: 'missing' }, { result: 'temperror' }))).toEqual({ text: 'Needs attention', tone: 'warning' });
        expect(senderDnsSummary(check({ status: 'unavailable' }, { result: 'fail' }))).toEqual({ text: 'Needs attention', tone: 'warning' });
        expect(senderDnsSummary(check({ status: 'unavailable' }))).toEqual({ text: "Couldn't check everything", tone: '' });
        expect(senderDnsSummary(check({}, { result: 'no_sending_ip', envelope_domain: undefined, envelope_kind: undefined }))).toEqual({ text: "Couldn't check everything", tone: '' });
        expect(senderDnsSummary(check())).toEqual({ text: 'All good', tone: 'success' });
    });

    it('counts monitoring and an SPF pass that does not line up as all good', () => {
        expect(senderDnsSummary(check({ status: 'monitoring', policy: 'none' }, { aligned: false }))).toEqual({ text: 'All good', tone: 'success' });
    });
});
