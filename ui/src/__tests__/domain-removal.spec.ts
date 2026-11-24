import { describe, expect, it } from 'vitest';

import { cleanupRecords, dependentSubdomains, parseDomainRemoval } from '@/domain-removal';
import type { Domain } from '@/openapi-client-generated';

const receipt = {
    name: 'mail.example.com',
    removed_at: '2026-11-23T12:00:00Z',
    dkim_records: [
        { selector: 'thinpost2', dns_name: 'thinpost2._domainkey.mail.example.com', keep_until: '2026-12-01T12:00:00Z' },
        { selector: 'thinpost3', dns_name: 'thinpost3._domainkey.mail.example.com' }
    ],
    bounce_domain: 'bounces.mail.example.com',
    tracking_domain: 'links.mail.example.com',
    keep_records_until: '2026-12-01T12:00:00Z'
};

describe('parseDomainRemoval', () => {
    it('reads a full receipt', () => {
        const view = parseDomainRemoval(receipt)!;
        expect(view.name).toBe('mail.example.com');
        expect(view.dkimRecords).toEqual([
            { selector: 'thinpost2', dnsName: 'thinpost2._domainkey.mail.example.com', keepUntil: '2026-12-01T12:00:00Z' },
            { selector: 'thinpost3', dnsName: 'thinpost3._domainkey.mail.example.com', keepUntil: null }
        ]);
        expect(view.bounceDomain).toBe('bounces.mail.example.com');
    });

    it('treats a null or absent keep_until as never used', () => {
        const view = parseDomainRemoval({ ...receipt, dkim_records: [{ selector: 'thinpost1', dns_name: 'thinpost1._domainkey.mail.example.com', keep_until: null }], bounce_domain: undefined, tracking_domain: null, keep_records_until: undefined });
        expect(view?.dkimRecords[0]?.keepUntil).toBeNull();
        expect(view?.bounceDomain).toBeNull();
        expect(view?.trackingDomain).toBeNull();
    });

    it.each([
        ['not an object', 'x'],
        ['null', null],
        ['no name', { ...receipt, name: '' }],
        ['bad removed_at', { ...receipt, removed_at: 'yesterday' }],
        ['records not an array', { ...receipt, dkim_records: {} }],
        ['record without a name', { ...receipt, dkim_records: [{ selector: 'thinpost1' }] }],
        ['record with a bad date', { ...receipt, dkim_records: [{ selector: 'thinpost1', dns_name: 'x', keep_until: 'soon' }] }],
        ['bounce not a string', { ...receipt, bounce_domain: 5 }],
        ['bad keep_records_until', { ...receipt, keep_records_until: 'later' }]
    ])('rejects %s', (_, value) => {
        expect(parseDomainRemoval(value)).toBeNull();
    });
});

describe('cleanupRecords', () => {
    it('lists keys, then the bounce CNAME, then the tracking name', () => {
        const rows = cleanupRecords(parseDomainRemoval(receipt)!);
        expect(rows.map(r => r.name)).toEqual([
            'thinpost2._domainkey.mail.example.com',
            'thinpost3._domainkey.mail.example.com',
            'bounces.mail.example.com',
            'links.mail.example.com'
        ]);
        expect(rows[0]?.advice).toMatch(/^keep until .*2026/);
        expect(rows[1]?.advice).toBe('never used, delete now');
        expect(rows[2]?.advice).toMatch(/^delete the CNAME after .*2026/);
        expect(rows[3]?.advice).toContain('old links matter');
    });

    it('is empty when nothing needs cleaning', () => {
        expect(cleanupRecords(parseDomainRemoval({ name: 'mail.example.com', removed_at: '2026-11-23T12:00:00Z', dkim_records: [] })!)).toEqual([]);
    });
});

describe('dependentSubdomains', () => {
    const at = '2026-10-01T12:00:00Z';
    const bounce = { name: 'b.mail.example.com', state: 'active', checking: false, created_at: at };
    const tracking = { name: 'l.mail.example.com', state: 'active', checking: false, created_at: at };
    const domain = (name: string, patch: object = {}) => ({ name, active: true, created_at: at, ...patch }) as Domain;

    it('finds subdomains without their own bounce or tracking domain', () => {
        const parent = domain('mail.example.com', { bounce_domain: bounce, tracking_domain: tracking });
        const all = [
            parent,
            domain('a.mail.example.com'),
            domain('b.mail.example.com', { bounce_domain: bounce }),
            domain('c.mail.example.com', { bounce_domain: bounce, tracking_domain: tracking }),
            domain('unverified.mail.example.com', { active: false }),
            domain('other.example.com'),
            domain('xmail.example.com')
        ];
        expect(dependentSubdomains(parent, all)).toEqual([
            { name: 'a.mail.example.com', bounce: true, tracking: true },
            { name: 'b.mail.example.com', bounce: false, tracking: true }
        ]);
    });

    it('finds none when the domain has neither', () => {
        const parent = domain('mail.example.com');
        expect(dependentSubdomains(parent, [parent, domain('a.mail.example.com')])).toEqual([]);
    });

    it('skips a subdomain that a nearer parent serves, as mail picks the most specific', () => {
        const top = domain('example.com', { bounce_domain: bounce });
        const mid = domain('mid.example.com', { bounce_domain: bounce });
        const all = [top, mid, domain('a.mid.example.com'), domain('b.example.com')];
        expect(dependentSubdomains(top, all)).toEqual([{ name: 'b.example.com', bounce: true, tracking: false }]);
        expect(dependentSubdomains(mid, all)).toEqual([{ name: 'a.mid.example.com', bounce: true, tracking: false }]);
    });

    it('ignores a bounce or tracking domain that mail does not use yet', () => {
        const parent = domain('mail.example.com', { bounce_domain: { ...bounce, state: 'failing' }, tracking_domain: { ...tracking, state: 'pending' } });
        expect(dependentSubdomains(parent, [parent, domain('a.mail.example.com')])).toEqual([]);
        // a subdomain's own failing bounce domain doesn't stop it using the parent's
        const serving = domain('mail.example.com', { bounce_domain: bounce });
        const sub = domain('a.mail.example.com', { bounce_domain: { ...bounce, state: 'pending' } });
        expect(dependentSubdomains(serving, [serving, sub])).toEqual([{ name: 'a.mail.example.com', bounce: true, tracking: false }]);
    });

    it('ignores a tracking tombstone', () => {
        const parent = domain('mail.example.com', { tracking_domain: { ...tracking, state: 'removed' } });
        expect(dependentSubdomains(parent, [parent, domain('a.mail.example.com')])).toEqual([]);
    });
});
