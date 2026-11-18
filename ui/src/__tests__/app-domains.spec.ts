import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, networkError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppDomains: vi.fn(),
    requestAppDomain: vi.fn(),
    verifyAppDomain: vi.fn(),
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
const pending = { name: 'mail.example.com', active: false, created_at: '2026-10-02T12:00:00Z' };
const verified = { ...pending, active: true, verified_at: '2026-10-03T12:00:00Z' };
const CHALLENGE = { domain: pending, dns_name: '_thinpost.mail.example.com', dns_value: 'thinpost-verification=abc123' };

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

    it('distinguishes ownership verification from DKIM/SPF readiness', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [verified] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        const help = await openHelp(wrapper!, 'sending domains');
        expect(help).toContain('Verified means you own the domain, not that mail is ready to send');
        expect(help).toContain("Verifying ownership doesn't set up or check DKIM, SPF, DMARC or reverse DNS");
        expect(help).toContain('Sending also needs a DKIM key, which your Thinpost administrator sets up.');
        const text = wrapper!.text();
        expect(text).toContain('Verified');
        expect(text).toContain('Not shown');
        // A verified domain has no verify or reissue actions; the API rejects reissue for active domains.
        expect(hasButton('Verify')).toBe(false);
        expect(hasButton('New TXT record')).toBe(false);
    });

    it('requests a TXT record with a normalized domain and shows copyable TXT values', async () => {
        api.listAppDomains.mockResolvedValueOnce({ data: { domains: [] } }).mockResolvedValue({ data: { domains: [pending] } });
        api.requestAppDomain.mockResolvedValue({ data: CHALLENGE });
        const writeText = vi.fn().mockResolvedValue(undefined);
        Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
        const setItem = vi.spyOn(Storage.prototype, 'setItem');

        await mountAt(`/apps/${APP_ID}/domains`);
        await submitDomain('  Mail.Example.COM ');

        expect(api.requestAppDomain).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { domain: 'mail.example.com' } });
        expect(inputValues()).toEqual([CHALLENGE.dns_name, CHALLENGE.dns_value]);
        expect(wrapper!.text()).toContain('Not verified');
        expect(api.listAppDomains).toHaveBeenCalledTimes(2);

        await button('Copy Record value').trigger('click');
        await flushPromises();
        expect(writeText).toHaveBeenCalledWith(CHALLENGE.dns_value);
        expect(wrapper!.text()).toContain('Copied to clipboard.');

        expect(setItem.mock.calls.flat().join(' ')).not.toContain(CHALLENGE.dns_value);
        setItem.mockRestore();
    });

    it('rejects an invalid domain without calling the API', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        await submitDomain('bücher.example');
        expect(api.requestAppDomain).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('xn-- form');
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
    });

    it('explains a 409 from verification as a non-matching TXT record', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        api.verifyAppDomain.mockRejectedValue(httpError(409));

        await mountAt(`/apps/${APP_ID}/domains`);
        await button('Verify').trigger('click');
        await flushPromises();

        expect(wrapper!.text()).toContain('TXT records exist at _thinpost.mail.example.com, but none matches the current value');
        expect(vf.showToast).not.toHaveBeenCalled();
    });

    it('explains a 503 from verification as a missing or unpropagated record, or a failed lookup', async () => {
        // The API maps any TXT lookup error, including NXDOMAIN for an unpublished record, to 503.
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        api.verifyAppDomain.mockRejectedValue(httpError(503));

        await mountAt(`/apps/${APP_ID}/domains`);
        await button('Verify').trigger('click');
        await flushPromises();

        expect(wrapper!.text()).toContain("Thinpost can't see a TXT record at _thinpost.mail.example.com yet");
        expect(wrapper!.text()).toContain('DNS changes can take a while to spread');
        expect(api.listAppDomains).toHaveBeenCalledTimes(2);
    });

    it('reports an uncertain outcome when verification gets no response', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        api.verifyAppDomain.mockRejectedValue(networkError());
        await mountAt(`/apps/${APP_ID}/domains`);
        await button('Verify').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("We can't tell whether the change went through");
    });

    it('confirms before re-adding an unverified domain, because that replaces its TXT record', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        api.requestAppDomain.mockResolvedValue({ data: CHALLENGE });
        await mountAt(`/apps/${APP_ID}/domains`);

        vf.showConfirm.mockResolvedValueOnce(false);
        await submitDomain('mail.example.com');
        expect(vf.showConfirm).toHaveBeenCalledWith('Request a new TXT record?', expect.stringContaining('the current one will stop working'));
        expect(api.requestAppDomain).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(true);
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.requestAppDomain).toHaveBeenCalledTimes(1);
        expect(inputValues()).toEqual([CHALLENGE.dns_name, CHALLENGE.dns_value]);
    });

    it('requests a new TXT record only after confirmation and replaces the shown value', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        api.requestAppDomain.mockResolvedValue({ data: { ...CHALLENGE, dns_value: 'thinpost-verification=new' } });

        await mountAt(`/apps/${APP_ID}/domains`);
        vf.showConfirm.mockResolvedValueOnce(false);
        await button('New TXT record').trigger('click');
        await flushPromises();
        expect(api.requestAppDomain).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(true);
        await button('New TXT record').trigger('click');
        await flushPromises();
        expect(api.requestAppDomain).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { domain: 'mail.example.com' } });
        expect(inputValues()).toEqual([CHALLENGE.dns_name, 'thinpost-verification=new']);
    });

    it('reports an already-verified conflict when reissuing', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        api.requestAppDomain.mockRejectedValue(httpError(409));
        vf.showConfirm.mockResolvedValue(true);

        await mountAt(`/apps/${APP_ID}/domains`);
        await button('New TXT record').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('already verified');
        expect(inputValues()).toEqual([]);
    });

    it('reports an uncertain outcome when the TXT record request gets no response', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [] } });
        api.requestAppDomain.mockRejectedValue(networkError());
        await mountAt(`/apps/${APP_ID}/domains`);
        await submitDomain('mail.example.com');
        expect(wrapper!.text()).toContain("We can't tell whether the change went through");
        expect(inputValues()).toEqual([]);
    });

    it('treats a TXT record response without a TXT value as invalid', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [] } });
        api.requestAppDomain.mockResolvedValue({ data: { domain: pending, dns_name: CHALLENGE.dns_name } });
        await mountAt(`/apps/${APP_ID}/domains`);
        await submitDomain('mail.example.com');
        expect(wrapper!.text()).toContain("something the console didn't understand");
        expect(inputValues()).toEqual([]);
    });

    it('does not offer TXT record requests for an inactive app', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        api.listAppDomains.mockResolvedValue({ data: { domains: [pending] } });
        await mountAt(`/apps/${APP_ID}/domains`);
        expect(button('Add domain').attributes('disabled')).toBeDefined();
        expect(button('New TXT record').attributes('disabled')).toBeDefined();
        expect(button('Verify').attributes('disabled')).toBeUndefined();
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
