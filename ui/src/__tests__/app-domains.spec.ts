import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, networkError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppDomains: vi.fn(),
    requestAppDomain: vi.fn(),
    verifyAppDomain: vi.fn(),
    createAppDomainDkimKey: vi.fn(),
    activateAppDomainDkimKey: vi.fn(),
    cancelAppDomainDkimKey: vi.fn(),
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

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    localStorage.clear();
    sessionStorage.clear();
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
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
        expect(help).toContain("Verifying doesn't set up or check SPF, DMARC or reverse DNS");
        expect(help).not.toContain('_thinpost');
        const text = wrapper!.text();
        expect(text).toContain('Verified');
        expect(text).toContain('thinpost1');
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
