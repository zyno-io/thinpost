import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, networkError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppDomains: vi.fn(),
    requestAppDomain: vi.fn(),
    verifyAppDomain: vi.fn(),
    checkAppDomainDns: vi.fn(),
    createAppDomainDkimKey: vi.fn(),
    activateAppDomainDkimKey: vi.fn(),
    cancelAppDomainDkimKey: vi.fn(),
    setAppBounceDomain: vi.fn(),
    checkAppBounceDomain: vi.fn(),
    deleteAppBounceDomain: vi.fn(),
    setAppTrackingDomain: vi.fn(),
    checkAppTrackingDomain: vi.fn(),
    deleteAppTrackingDomain: vi.fn(),
    getRuntime: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({
    showConfirm: vi.fn(),
    showToast: vi.fn()
}));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { default: AppDomains } = await import('@/screens/app-domains.vue');
const { resetRuntimeFeatures } = await import('@/runtime-features');

const APP_ID = '0123456789abcdef0123456789abcdef';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const LONG_VALUE = `v=DKIM1; k=rsa; p=${'A'.repeat(392)}`;
const keyOne = { selector: 'thinpost1', dns_name: 'thinpost1._domainkey.mail.example.com', dns_value: LONG_VALUE, activated_at: '2026-10-03T12:00:00Z' };
const keyTwo = { selector: 'thinpost2', dns_name: 'thinpost2._domainkey.mail.example.com', dns_value: `${LONG_VALUE}B` };
const base = { name: 'mail.example.com', created_at: '2026-10-02T12:00:00Z' };
// Not verified, first key waiting for its record.
const pending = { ...base, active: false, pending_key: { ...keyOne, activated_at: undefined } };
const verified = { ...base, active: true, verified_at: '2026-10-03T12:00:00Z', active_key: keyOne };
// Verified before keys existed: no key at all.
const noKey = { ...base, active: true, verified_at: '2026-10-03T12:00:00Z' };
const rotating = { ...verified, pending_key: keyTwo };
const CHALLENGE = { domain: pending, dns_name: keyOne.dns_name, dns_value: LONG_VALUE };

let wrapper: VueWrapper | undefined;

async function mountAt(path: string) {
    const mounted = await mountAppRoute(path, { 'app-domains': AppDomains });
    wrapper = mounted.wrapper;
    return wrapper;
}

function button(label: string) {
    const match = wrapper!.findAll('button').find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}

function hasButton(label: string) {
    return wrapper!.findAll('button').some(b => b.text().includes(label));
}

function inputValues() {
    return wrapper!.findAll<HTMLInputElement>('input[readonly]').map(i => i.element.value);
}

async function submitDomain(value: string) {
    await button('Add domain').trigger('click');
    await wrapper!.find('input[type="text"]').setValue(value);
    await wrapper!.find('form').trigger('submit');
    await flushPromises();
}

function senderDnsCheck(domain: string) {
    return {
        domain,
        dmarc: { status: 'ok', org_domain: 'example.com', record_domain: 'example.com', record: 'v=DMARC1; p=reject', policy: 'reject', aspf: 'r' },
        spf: { result: 'pass', envelope_domain: 'smtp.example.net', envelope_kind: 'standard', aligned: false }
    };
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    localStorage.clear();
    sessionStorage.clear();
    resetRuntimeFeatures();
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.checkAppDomainDns.mockImplementation(async ({ path }: { path: { domain: string } }) => ({ data: senderDnsCheck(path.domain) }));
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('sending domains screen', () => {
    it('does not call the API for a malformed app ID', async () => {
        await mountAt('/apps/nope/domains');
        expect(wrapper!.text()).toContain('App not found');
        expect(api.listAppDomains).not.toHaveBeenCalled();
    });

    it('explains that the DKIM record verifies the domain', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        const help = await openHelp(wrapper!, 'sending domains');
        expect(help).toContain('Verified means Thinpost can see your DKIM record');
        expect(help).toContain("Verifying doesn't set up SPF, DMARC or reverse DNS");
        expect(help).toContain('The SPF and DMARC panel under each verified domain');
        expect(help).not.toContain('_thinpost');
        const text = wrapper!.text();
        expect(text).toContain('Verified');
        expect(text).toContain('thinpost1');
        // The selector and "in use" are separate words, not run together.
        expect(wrapper!.find('td.key-cell').text()).toBe('thinpost1 in use');
        // A verified domain with a key offers a test send and a rotation, not verification.
        expect(hasButton('Send test')).toBe(true);
        expect(hasButton('Rotate key')).toBe(true);
        expect(hasButton('Verify')).toBe(false);
    });

    it('adds a domain and shows its persistent DKIM record with a split-strings copy option', async () => {
        api.listAppDomains.mockResolvedValueOnce({ data: { domains: [] } }).mockResolvedValue({ data: { domains: [pending] } });
        api.requestAppDomain.mockResolvedValue({ data: CHALLENGE });
        const writeText = vi.fn().mockResolvedValue(undefined);
        Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
        const setItem = vi.spyOn(Storage.prototype, 'setItem');

        await mountAt(`/apps/${APP_ID}/domains`);
        await submitDomain('  Mail.Example.COM ');

        expect(api.requestAppDomain).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { domain: 'mail.example.com' } });
        expect(inputValues()).toEqual([keyOne.dns_name, LONG_VALUE]);
        expect(wrapper!.text()).toContain('Waiting for DNS record');
        expect(wrapper!.text()).toContain('Keep exactly one record at this name.');
        expect(api.listAppDomains).toHaveBeenCalledTimes(2);

        await button('Copy Record value').trigger('click');
        await flushPromises();
        expect(writeText).toHaveBeenLastCalledWith(LONG_VALUE);
        expect(wrapper!.text()).toContain('Copied to clipboard.');

        // The split option shows and copies the value as quoted strings of at most 255 characters.
        await wrapper!.find('input[type="checkbox"]').setValue(true);
        const split = inputValues()[1];
        expect(split).toBe(`"${LONG_VALUE.slice(0, 255)}" "${LONG_VALUE.slice(255)}"`);
        await button('Copy Record value (split into 255-character strings)').trigger('click');
        await flushPromises();
        expect(writeText).toHaveBeenLastCalledWith(split);

        expect(setItem.mock.calls.flat().join(' ')).not.toContain(LONG_VALUE);
        setItem.mockRestore();
    });

    it('rejects an invalid domain without calling the API', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        await submitDomain('bücher.example');
        expect(api.requestAppDomain).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('xn-- form');
    });

    it('shows the record of a domain that is waiting, even after a reload', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(inputValues()).toEqual([keyOne.dns_name, LONG_VALUE]);
    });

    it('verifies a pending domain and reloads its status from the API', async () => {
        api.listAppDomains.mockResolvedValueOnce({ data: { domains: [pending] } }).mockResolvedValue({ data: { domains: [verified] } });
        api.verifyAppDomain.mockResolvedValue({ data: verified });

        await mountAt(`/apps/${APP_ID}/domains`);
        await button('Verify').trigger('click');
        await flushPromises();

        expect(api.verifyAppDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com' } });
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'mail.example.com verified' }));
        expect(hasButton('Verify')).toBe(false);
        expect(inputValues()).toEqual([]);
    });

    it.each([
        ['not_found', "Thinpost can't see a TXT record at thinpost1._domainkey.mail.example.com yet"],
        ['multiple_records', 'There is more than one TXT record at thinpost1._domainkey.mail.example.com. Keep exactly one'],
        ['different_key', "isn't the key shown here"],
        ['testing_mode', 'marked as a test (t=y)'],
        ['unsupported', "isn't a standard DKIM key record"]
    ])('explains a %s result from the DNS check', async (reason, expected) => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        api.verifyAppDomain.mockRejectedValue(httpError(409, { error: 'Conflict', reason }));

        await mountAt(`/apps/${APP_ID}/domains`);
        await button('Verify').trigger('click');
        await flushPromises();

        expect(wrapper!.text()).toContain(expected);
        expect(vf.showToast).not.toHaveBeenCalled();
    });

    it('explains a failed DNS lookup and a missing pending key', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        api.verifyAppDomain.mockRejectedValueOnce(httpError(503));
        await mountAt(`/apps/${APP_ID}/domains`);
        await button('Verify').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("Thinpost couldn't look up DNS just now");
        expect(api.listAppDomains).toHaveBeenCalledTimes(2);

        api.verifyAppDomain.mockRejectedValueOnce(httpError(409));
        await button('Verify').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('There is no new key waiting for a record');
    });

    it('reports an uncertain outcome when verification gets no response', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        api.verifyAppDomain.mockRejectedValue(networkError());
        await mountAt(`/apps/${APP_ID}/domains`);
        await button('Verify').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("We can't tell whether the change went through");
    });

    it('lets a domain verified before DKIM keys existed create one', async () => {
        api.listAppDomains.mockResolvedValueOnce({ data: { domains: [noKey] } }).mockResolvedValue({ data: { domains: [{ ...noKey, pending_key: keyOne }] } });
        api.createAppDomainDkimKey.mockResolvedValue({ data: { domain: noKey, dns_name: keyOne.dns_name, dns_value: LONG_VALUE } });

        await mountAt(`/apps/${APP_ID}/domains`);
        expect(wrapper!.text()).toContain('No DKIM key yet');
        expect(hasButton('Send test')).toBe(false);
        await button('Create DKIM key').trigger('click');
        await flushPromises();

        // A first key needs no confirmation.
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(api.createAppDomainDkimKey).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com' } });
        expect(inputValues()).toEqual([keyOne.dns_name, LONG_VALUE]);
        expect(hasButton('Verify')).toBe(true);
    });

    it('rotates a key: confirm, create, check, switch', async () => {
        api.listAppDomains
            .mockResolvedValueOnce({ data: { domains: [verified] } })
            .mockResolvedValueOnce({ data: { domains: [rotating] } })
            .mockResolvedValueOnce({ data: { domains: [{ ...rotating, pending_key: { ...keyTwo, published_at: '2026-10-04T12:00:00Z' } }] } })
            .mockResolvedValue({ data: { domains: [{ ...verified, active_key: { ...keyTwo, activated_at: '2026-10-05T12:00:00Z' }, retired_key: { selector: 'thinpost1', keep_record_until: '2026-10-12T12:00:00Z' } }] } });
        api.createAppDomainDkimKey.mockResolvedValue({ data: { domain: rotating, dns_name: keyTwo.dns_name, dns_value: keyTwo.dns_value } });
        api.verifyAppDomain.mockResolvedValue({ data: verified });
        api.activateAppDomainDkimKey.mockResolvedValue({ data: verified });
        await mountAt(`/apps/${APP_ID}/domains`);

        vf.showConfirm.mockResolvedValueOnce(false);
        await button('Rotate key').trigger('click');
        await flushPromises();
        expect(api.createAppDomainDkimKey).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(true);
        await button('Rotate key').trigger('click');
        await flushPromises();
        expect(api.createAppDomainDkimKey).toHaveBeenCalledTimes(1);
        expect(wrapper!.text()).toContain('New DKIM record for mail.example.com');
        expect(wrapper!.text()).toContain('New key thinpost2: waiting for its DNS record.');
        expect(inputValues()).toEqual([keyTwo.dns_name, keyTwo.dns_value]);

        await button('Check record').trigger('click');
        await flushPromises();
        expect(api.verifyAppDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com' } });
        expect(wrapper!.text()).toContain('New key thinpost2: record found, ready to switch.');

        vf.showConfirm.mockResolvedValueOnce(true);
        await button('Switch').trigger('click');
        await flushPromises();
        expect(vf.showConfirm).toHaveBeenLastCalledWith('Switch to the new key?', expect.stringContaining('the current key is deleted for good'));
        expect(api.activateAppDomainDkimKey).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com', selector: 'thinpost2' } });
        expect(wrapper!.text()).toContain('Old record thinpost1 can stay in DNS until');
        expect(hasButton('Switch')).toBe(false);
    });

    it('does not switch when the new record is not in DNS yet', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [rotating] } });
        api.activateAppDomainDkimKey.mockRejectedValue(httpError(409, { error: 'Conflict', reason: 'not_found' }));
        vf.showConfirm.mockResolvedValue(true);
        await mountAt(`/apps/${APP_ID}/domains`);
        await button('Switch').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("Thinpost can't see a TXT record at thinpost2._domainkey.mail.example.com yet");
        expect(vf.showToast).not.toHaveBeenCalled();
    });

    it('cancels a pending rotation only after confirmation', async () => {
        api.listAppDomains.mockResolvedValueOnce({ data: { domains: [rotating] } }).mockResolvedValue({ data: { domains: [verified] } });
        api.cancelAppDomainDkimKey.mockResolvedValue({});
        await mountAt(`/apps/${APP_ID}/domains`);

        vf.showConfirm.mockResolvedValueOnce(false);
        await button('Cancel').trigger('click');
        await flushPromises();
        expect(api.cancelAppDomainDkimKey).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(true);
        await button('Cancel').trigger('click');
        await flushPromises();
        expect(api.cancelAppDomainDkimKey).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com', selector: 'thinpost2' } });
        expect(hasButton('Switch')).toBe(false);
        expect(inputValues()).toEqual([]);
    });

    it('shows and hides the current record of a verified domain on request', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(inputValues()).toEqual([]);
        await button('Show record').trigger('click');
        expect(inputValues()).toEqual([keyOne.dns_name, LONG_VALUE]);
        await button('Hide record').trigger('click');
        expect(inputValues()).toEqual([]);
    });

    it('explains an already-keyed domain when adding it again', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        api.requestAppDomain.mockRejectedValue(httpError(409));
        await mountAt(`/apps/${APP_ID}/domains`);
        await submitDomain('mail.example.com');
        expect(wrapper!.text()).toContain('already has a DKIM key');
    });

    it('reports an uncertain outcome when the request gets no response', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [] } });
        api.requestAppDomain.mockRejectedValue(networkError());
        await mountAt(`/apps/${APP_ID}/domains`);
        await submitDomain('mail.example.com');
        expect(wrapper!.text()).toContain("We can't tell whether the change went through");
        expect(inputValues()).toEqual([]);
    });

    it('treats a response without a DKIM record as invalid', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [] } });
        api.requestAppDomain.mockResolvedValue({ data: { domain: pending, dns_name: CHALLENGE.dns_name } });
        await mountAt(`/apps/${APP_ID}/domains`);
        await submitDomain('mail.example.com');
        expect(wrapper!.text()).toContain("something the console didn't understand");
    });

    it('does not offer new keys for an inactive app', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified, { ...noKey, name: 'b.example.com' }] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(button('Add domain').attributes('disabled')).toBeDefined();
        expect(button('Rotate key').attributes('disabled')).toBeDefined();
        expect(button('Create DKIM key').attributes('disabled')).toBeDefined();
    });

    it('disables mutations once the session has ended', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        authState.status = 'ended';
        await flushPromises();
        expect(button('Verify').attributes('disabled')).toBeDefined();
        expect(button('Add domain').attributes('disabled')).toBeDefined();
    });

    it('shows an error instead of an empty list for a malformed response', async () => {
        api.listAppDomains.mockResolvedValue({ data: '<!doctype html>' });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(wrapper!.text()).toContain("something the console didn't understand");
        expect(wrapper!.text()).not.toContain('No sending domains');
    });
});

describe('bounce domains', () => {
    const TARGET = { cname_target: 'smtp.example.net', cname_target_status: 'ok' };
    const none = { ...verified, ...TARGET };
    const bounce = (patch: object = {}) => ({ name: 'bounces.mail.example.com', state: 'pending', checking: false, created_at: '2026-11-21T12:00:00Z', ...patch });
    const withBounce = (patch: object = {}) => ({ ...verified, ...TARGET, bounce_domain: bounce(patch) });

    function bouncePanel() {
        return wrapper!.find('[data-testid="bounce-mail.example.com"]');
    }

    it('offers a bounce domain on a verified domain, and not on one that is not verified', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending, { ...verified, name: 'ok.example.com' }] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(wrapper!.find('[data-testid="bounce-mail.example.com"]').exists()).toBe(false);
        expect(wrapper!.find('[data-testid="bounce-ok.example.com"]').exists()).toBe(true);
        expect(wrapper!.text()).toContain("Mail from ok.example.com uses Thinpost's standard bounce address");
        const help = await openHelp(wrapper!, 'bounce domains');
        expect(help).toContain('DNS only, not proxied');
        expect(help).toContain('aspf=s');
        expect(help).toContain('different IP');
    });

    it('adds a bounce domain after checking the name against the sending domain', async () => {
        api.listAppDomains.mockResolvedValueOnce({ data: { domains: [none] } }).mockResolvedValue({ data: { domains: [withBounce()] } });
        api.setAppBounceDomain.mockResolvedValue({ data: bounce() });
        await mountAt(`/apps/${APP_ID}/domains`);

        await bouncePanel().findAll('button').find(b => b.text().includes('Add bounce domain'))!.trigger('click');
        const input = () => wrapper!.find<HTMLInputElement>('input[placeholder="bounces.mail.example.com"]');
        await input().setValue('bounces.example.org');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.setAppBounceDomain).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('Use a subdomain of mail.example.com');

        await input().setValue(' Bounces.Mail.Example.com ');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.setAppBounceDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com' }, body: { name: 'bounces.mail.example.com' } });
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Bounce domain saved for mail.example.com' }));
        // The list is read again and the CNAME to add is shown, DNS only.
        expect(api.listAppDomains).toHaveBeenCalledTimes(2);
        expect(bouncePanel().text()).toContain('Waiting for DNS record');
        expect(inputValues()).toContain('smtp.example.net');
        expect(inputValues()).toContain('bounces.mail.example.com');
        expect(bouncePanel().text()).toContain('DNS only, not proxied');
    });

    it.each([
        [400, 'Use a subdomain of mail.example.com, such as bounces.mail.example.com'],
        [409, "can't be used"],
        [503, 'temporarily unavailable']
    ])('explains a %s answer when saving', async (status, expected) => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [none] } });
        api.setAppBounceDomain.mockRejectedValue(httpError(status));
        await mountAt(`/apps/${APP_ID}/domains`);
        await bouncePanel().findAll('button').find(b => b.text().includes('Add bounce domain'))!.trigger('click');
        await wrapper!.find<HTMLInputElement>('input[placeholder="bounces.mail.example.com"]').setValue('bounces.mail.example.com');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(wrapper!.text()).toContain(expected);
        expect(vf.showToast).not.toHaveBeenCalled();
    });

    it('shows an active domain as in use, without the record to add', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [withBounce({ state: 'active', result: 'ok', checked_at: '2026-11-21T12:30:00Z' })] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(bouncePanel().find('[data-testid="bounce-status"]').text()).toBe('In use');
        expect(bouncePanel().find('[data-testid="bounce-message"]').text()).toContain('SPF passes for your domain too');
        expect(inputValues()).toEqual([]);
        expect(bouncePanel().text()).toContain('Last checked');
    });

    it('checks now and reads the list again', async () => {
        api.listAppDomains.mockResolvedValueOnce({ data: { domains: [withBounce()] } }).mockResolvedValue({ data: { domains: [withBounce({ state: 'active', result: 'ok' })] } });
        api.checkAppBounceDomain.mockResolvedValue({ data: bounce({ state: 'active', result: 'ok' }) });
        await mountAt(`/apps/${APP_ID}/domains`);
        await bouncePanel().findAll('button').find(b => b.text().includes('Check now'))!.trigger('click');
        await flushPromises();
        expect(api.checkAppBounceDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com' } });
        expect(bouncePanel().find('[data-testid="bounce-status"]').text()).toBe('In use');
    });

    it.each([
        [503, "couldn't look up DNS just now"],
        [409, 'Another check was already running']
    ])('explains a %s answer from a check', async (status, expected) => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [withBounce()] } });
        api.checkAppBounceDomain.mockRejectedValue(httpError(status));
        await mountAt(`/apps/${APP_ID}/domains`);
        await bouncePanel().findAll('button').find(b => b.text().includes('Check now'))!.trigger('click');
        await flushPromises();
        expect(bouncePanel().text()).toContain(expected);
    });

    it('explains a failing domain for each result, without inventing a target', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [{ ...withBounce({ state: 'failing', result: 'other_ip' }), cname_target: 'smtp2.example.net' }] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(bouncePanel().find('[data-testid="bounce-status"]').text()).toBe('Not in use');
        expect(bouncePanel().find('[data-testid="bounce-message"]').text()).toContain('Point the CNAME at smtp2.example.net.');
        wrapper!.unmount();

        api.listAppDomains.mockResolvedValue({ data: { domains: [{ ...verified, cname_target_status: 'dns_unconfirmed', bounce_domain: bounce({ state: 'failing', result: 'missing' }) }] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(bouncePanel().find('[data-testid="bounce-message"]').text()).toContain("can't confirm the name to point at");
        expect(inputValues()).toEqual([]);
    });

    it('removes a bounce domain only after confirming', async () => {
        api.listAppDomains.mockResolvedValueOnce({ data: { domains: [withBounce({ state: 'active', result: 'ok' })] } }).mockResolvedValue({ data: { domains: [none] } });
        api.deleteAppBounceDomain.mockResolvedValue({});
        await mountAt(`/apps/${APP_ID}/domains`);
        const remove = () => bouncePanel().findAll('button').find(b => b.text().includes('Remove'))!;

        vf.showConfirm.mockResolvedValueOnce(false);
        await remove().trigger('click');
        await flushPromises();
        expect(api.deleteAppBounceDomain).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(true);
        await remove().trigger('click');
        await flushPromises();
        expect(vf.showConfirm).toHaveBeenLastCalledWith('Remove the bounce domain?', expect.stringContaining("standard bounce address"));
        expect(api.deleteAppBounceDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com' } });
        expect(bouncePanel().text()).toContain("uses Thinpost's standard bounce address");
    });

    it('does not let a paused app change its bounce domain', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        api.listAppDomains.mockResolvedValue({ data: { domains: [withBounce()] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        const change = bouncePanel().findAll('button').find(b => b.text().includes('Change'))!;
        expect(change.attributes('disabled')).toBeDefined();
    });
});

describe('tracking domains', () => {
    const runtime = (patch: object = {}) => ({
        data: { features: { submission: true, tracking: true }, tracking: { origin: 'https://links.example.net', link_event_limit: 25, custom_domains: true, cname_target: 'links.example.net', ...patch } }
    });
    const tracking = (patch: object = {}) => ({ name: 'links.mail.example.com', state: 'pending', checking: false, created_at: '2026-11-21T12:00:00Z', ...patch });
    const withTracking = (patch: object = {}) => ({ ...verified, tracking_domain: tracking(patch) });

    function trackingPanel() {
        return wrapper!.find('[data-testid="tracking-mail.example.com"]');
    }
    const panelButton = (label: string) => trackingPanel().findAll('button').find(b => b.text().includes(label))!;

    beforeEach(() => {
        api.getRuntime.mockResolvedValue(runtime());
    });

    it('offers a tracking domain on a verified domain, and not on one that is not verified', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending, { ...verified, name: 'ok.example.com' }] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(wrapper!.find('[data-testid="tracking-mail.example.com"]').exists()).toBe(false);
        expect(wrapper!.find('[data-testid="tracking-ok.example.com"]').exists()).toBe(true);
        expect(wrapper!.text()).toContain("use Thinpost's standard tracking address");
        const help = await openHelp(wrapper!, 'tracking domains');
        expect(help).toContain('Cloudflare');
        expect(help).toContain('One-click unsubscribe');
        expect(help).toContain('certificate');
    });

    it('shows no tracking panel when tracking is off or the runtime is unknown', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        api.getRuntime.mockResolvedValue({ data: { features: { submission: true, tracking: false } } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(trackingPanel().exists()).toBe(false);
        wrapper!.unmount();
        resetRuntimeFeatures();
        api.getRuntime.mockRejectedValue(networkError());
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(trackingPanel().exists()).toBe(false);
    });

    it('offers no add button when custom domains are off, but still shows an existing one', async () => {
        api.getRuntime.mockResolvedValue(runtime({ custom_domains: false }));
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(trackingPanel().exists()).toBe(false);
        wrapper!.unmount();
        resetRuntimeFeatures();
        api.listAppDomains.mockResolvedValue({ data: { domains: [withTracking({ state: 'active', https_result: 'ok' })] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(trackingPanel().exists()).toBe(true);
        expect(hasButton('Add tracking domain')).toBe(false);
        expect(panelButton('Change')).toBeUndefined();
        expect(panelButton('Remove')).toBeDefined();
        // Even an active row is not in use, and no record to add is offered.
        expect(trackingPanel().find('[data-testid="tracking-status"]').text()).toBe('Not in use');
        expect(trackingPanel().find('[data-testid="tracking-message"]').text()).toContain("this installation doesn't use your own tracking address right now");
        expect(inputValues()).toEqual([]);
        expect(trackingPanel().text()).not.toContain('CNAME record');
        wrapper!.unmount();
        resetRuntimeFeatures();
        api.listAppDomains.mockResolvedValue({ data: { domains: [withTracking({ state: 'pending' })] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(trackingPanel().find('[data-testid="tracking-status"]').text()).toBe('Not in use');
        expect(inputValues()).toEqual([]);
    });

    it('adds a tracking domain after checking the name against the sending domain', async () => {
        api.listAppDomains.mockResolvedValueOnce({ data: { domains: [verified] } }).mockResolvedValue({ data: { domains: [withTracking()] } });
        api.setAppTrackingDomain.mockResolvedValue({ data: tracking() });
        await mountAt(`/apps/${APP_ID}/domains`);

        await panelButton('Add tracking domain').trigger('click');
        const input = () => wrapper!.find<HTMLInputElement>('input[placeholder="links.mail.example.com"]');
        await input().setValue('links.example.org');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.setAppTrackingDomain).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('Use a subdomain of mail.example.com');

        await input().setValue(' Links.Mail.Example.com ');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.setAppTrackingDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com' }, body: { name: 'links.mail.example.com' } });
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Tracking domain saved for mail.example.com' }));
        expect(api.listAppDomains).toHaveBeenCalledTimes(2);
        // The record to add shows the deployment's tracking host.
        expect(trackingPanel().text()).toContain('Waiting for DNS record');
        expect(inputValues()).toContain('links.example.net');
        expect(inputValues()).toContain('links.mail.example.com');
    });

    it.each([
        [400, 'Use a subdomain of mail.example.com, such as links.mail.example.com'],
        [409, "can't be used"],
        [503, 'temporarily unavailable']
    ])('explains a %s answer when saving', async (status, expected) => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        api.setAppTrackingDomain.mockRejectedValue(httpError(status));
        await mountAt(`/apps/${APP_ID}/domains`);
        await panelButton('Add tracking domain').trigger('click');
        await wrapper!.find<HTMLInputElement>('input[placeholder="links.mail.example.com"]').setValue('links.mail.example.com');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(wrapper!.text()).toContain(expected);
        expect(vf.showToast).not.toHaveBeenCalled();
    });

    it('tells the waiting-for-a-secure-connection story once the DNS record is right', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [withTracking({ dns_result: 'ok', https_result: 'certificate' })] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(trackingPanel().find('[data-testid="tracking-status"]').text()).toBe('Waiting for a secure connection');
        const message = trackingPanel().find('[data-testid="tracking-message"]').text();
        expect(message).toContain('Your DNS record is right; your Thinpost administrator still needs to finish setting up this address.');
        expect(message).toContain('Links will start using it on their own once it works.');
    });

    it('shows an active domain as in use, with its certificate expiry, without the record to add', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [withTracking({ state: 'active', dns_result: 'wrong_target', https_result: 'ok', certificate_expires_at: '2027-01-02T03:04:05Z', checked_at: '2026-11-21T12:30:00Z' })] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(trackingPanel().find('[data-testid="tracking-status"]').text()).toBe('In use');
        // A proxied name never matches the DNS record and is still in use.
        expect(trackingPanel().find('[data-testid="tracking-message"]').text()).toContain("One-click unsubscribe links keep using Thinpost's standard address");
        expect(inputValues()).toEqual([]);
        expect(trackingPanel().text()).toContain('certificate expires');
        expect(trackingPanel().text()).toContain('Last checked');
    });

    it('checks now and reads the list again', async () => {
        api.listAppDomains.mockResolvedValueOnce({ data: { domains: [withTracking()] } }).mockResolvedValue({ data: { domains: [withTracking({ state: 'active', https_result: 'ok' })] } });
        api.checkAppTrackingDomain.mockResolvedValue({ data: tracking({ state: 'active', https_result: 'ok' }) });
        await mountAt(`/apps/${APP_ID}/domains`);
        await panelButton('Check now').trigger('click');
        await flushPromises();
        expect(api.checkAppTrackingDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com' } });
        expect(trackingPanel().find('[data-testid="tracking-status"]').text()).toBe('In use');
    });

    it.each([
        [503, "couldn't run the check just now"],
        [409, 'Another check was already running']
    ])('explains a %s answer from a check', async (status, expected) => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [withTracking()] } });
        api.checkAppTrackingDomain.mockRejectedValue(httpError(status));
        await mountAt(`/apps/${APP_ID}/domains`);
        await panelButton('Check now').trigger('click');
        await flushPromises();
        expect(trackingPanel().text()).toContain(expected);
    });

    it('removes a tracking domain only after confirming, and warns that old links need the DNS record and certificate', async () => {
        api.listAppDomains.mockResolvedValueOnce({ data: { domains: [withTracking({ state: 'active', https_result: 'ok' })] } }).mockResolvedValue({ data: { domains: [verified] } });
        api.deleteAppTrackingDomain.mockResolvedValue({});
        await mountAt(`/apps/${APP_ID}/domains`);

        vf.showConfirm.mockResolvedValueOnce(false);
        await panelButton('Remove').trigger('click');
        await flushPromises();
        expect(api.deleteAppTrackingDomain).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(true);
        await panelButton('Remove').trigger('click');
        await flushPromises();
        expect(vf.showConfirm).toHaveBeenLastCalledWith('Remove the tracking domain?', expect.stringContaining('Links already sent keep working only while the DNS record'));
        expect(vf.showConfirm.mock.lastCall?.[1]).toContain('New mail will use the standard tracking address');
        expect(api.deleteAppTrackingDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com' } });
        expect(trackingPanel().text()).toContain("use Thinpost's standard tracking address");
    });

    it('does not let a paused app change its tracking domain', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        api.listAppDomains.mockResolvedValue({ data: { domains: [withTracking()] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(panelButton('Change').attributes('disabled')).toBeDefined();
    });
});

describe('SPF and DMARC panel', () => {
    const goodCheck = senderDnsCheck('mail.example.com');
    const badCheck = {
        ...goodCheck,
        dmarc: { status: 'missing', org_domain: 'example.com' },
        spf: { result: 'fail', envelope_domain: 'smtp.example.net', envelope_kind: 'standard', aligned: false }
    };
    const checkButton = () => wrapper!.find('[data-testid="check-sender-dns"]');

    it('appears only for a verified domain with a key', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending, noKey, { ...verified, name: 'ok.example.com' }] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(wrapper!.findAll('[data-testid^="sender-dns-ok"]')).toHaveLength(1);
        expect(wrapper!.find('[data-testid="sender-dns-ok.example.com"]').text()).toContain('SPF and DMARC for ok.example.com');
        expect(wrapper!.findAll('[data-testid="check-sender-dns"]')).toHaveLength(1);
        expect(api.checkAppDomainDns).toHaveBeenCalledTimes(1);
        expect(checkButton().text()).toContain('SPF and DMARC for ok.example.com');
    });

    it('checks on mount and shows the summary and both results', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        api.checkAppDomainDns.mockResolvedValue({ data: badCheck });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(api.checkAppDomainDns).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'mail.example.com' } });
        expect(wrapper!.find('[data-testid="sender-dns-summary"]').text()).toBe('Needs attention');
        expect(wrapper!.find('[data-testid="dmarc-tag"]').text()).toBe('Missing');
        expect(wrapper!.find('[data-testid="dmarc-fix"]').text()).toContain('Add:');
        expect(wrapper!.find('[data-testid="dmarc-fix"]').text()).toContain('TXT _dmarc.example.com');
        expect(wrapper!.find('[data-testid="dmarc-fix-value"]').text()).toBe('v=DMARC1; p=none');
        expect(wrapper!.find('[data-testid="spf-tag"]').text()).toBe('Fails');
        expect(wrapper!.find('[data-testid="spf-envelope"]').text()).toBe('smtp.example.net');
        expect(wrapper!.find('[data-testid="spf-note"]').text()).toBe('Set by your Thinpost administrator, not on mail.example.com.');
        expect(checkButton().text()).toContain('Check again');
        expect(wrapper!.text()).not.toContain('This check only reads DNS');
    });

    it('says all good, with the record shown, and checks again on request', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        api.checkAppDomainDns.mockResolvedValue({ data: goodCheck });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(wrapper!.find('[data-testid="sender-dns-summary"]').text()).toBe('All good');
        expect(wrapper!.find('[data-testid="dmarc-tag"]').text()).toBe('Enforced');
        expect(wrapper!.find('[data-testid="dmarc-location"]').text()).toBe('_dmarc.example.com');
        expect(wrapper!.find('[data-testid="dmarc-record"]').text()).toBe('v=DMARC1; p=reject');
        expect(wrapper!.find('[data-testid="dmarc-fix"]').exists()).toBe(false);
        expect(wrapper!.find('[data-testid="spf-alignment"]').text()).toContain('Not aligned');
        await checkButton().trigger('click');
        await flushPromises();
        expect(api.checkAppDomainDns).toHaveBeenCalledTimes(2);
    });

    it('marks the problem parts, offers the corrected record, and copies it', async () => {
        const writeText = vi.fn().mockResolvedValue(undefined);
        Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        api.checkAppDomainDns.mockResolvedValue({ data: { ...goodCheck, dmarc: { status: 'invalid', org_domain: 'example.com', record_domain: 'example.com', record: 'v=DMARC1; p=block; rua=mailto:d@example.com' } } });
        await mountAt(`/apps/${APP_ID}/domains`);
        const parts = wrapper!.findAll('[data-testid="dmarc-record"] .part');
        expect(parts.map(p => p.attributes('data-state'))).toEqual(['ok', 'problem', 'ok']);
        expect(parts[1]?.attributes('title')).toBe('p must be none, quarantine or reject');
        expect(wrapper!.find('[data-testid="dmarc-fix"]').text()).toContain('Update:');
        expect(wrapper!.find('[data-testid="dmarc-fix-value"]').text()).toBe('v=DMARC1; p=none; rua=mailto:d@example.com');
        await wrapper!.find('[data-testid="dmarc-fix-copy"]').trigger('click');
        expect(writeText).toHaveBeenCalledWith('v=DMARC1; p=none; rua=mailto:d@example.com');
    });

    it('lists duplicate DMARC records', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        api.checkAppDomainDns.mockResolvedValue({ data: { ...goodCheck, dmarc: { status: 'multiple', org_domain: 'example.com', record_domain: 'example.com', records: ['v=DMARC1; p=none', 'v=DMARC1; p=reject'] } } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(wrapper!.findAll('[data-testid="dmarc-records"] li')).toHaveLength(2);
        expect(wrapper!.find('[data-testid="dmarc-fix"]').text()).toBe('Keep one; delete the others.');
    });

    it('explains an unavailable check', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        api.checkAppDomainDns.mockRejectedValue(httpError(503));
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(wrapper!.text()).toContain("Thinpost couldn't look up DNS just now");
        expect(wrapper!.find('[data-testid="sender-dns-summary"]').exists()).toBe(false);
    });

    it('does not block the DKIM actions while it runs', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        let finish!: (value: unknown) => void;
        api.checkAppDomainDns.mockReturnValue(new Promise(resolve => (finish = resolve)));
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(wrapper!.text()).toContain('Checking what receivers find for mail.example.com…');
        expect(checkButton().text()).toContain('Checking…');
        expect(checkButton().attributes('disabled')).toBeDefined();
        expect(button('Rotate key').attributes('disabled')).toBeUndefined();
        finish({ data: goodCheck });
        await flushPromises();
        expect(checkButton().text()).toContain('Check again');
    });

    it('treats an answer for another domain as invalid', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        api.checkAppDomainDns.mockResolvedValue({ data: { ...goodCheck, domain: 'other.example.com' } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(wrapper!.text()).toContain("something the console didn't understand");
        expect(wrapper!.find('[data-testid="sender-dns-summary"]').exists()).toBe(false);
    });
});
