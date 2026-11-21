import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppKeys: vi.fn(),
    issueAppKey: vi.fn(),
    revokeAppKey: vi.fn(),
    getPublicConfig: vi.fn(),
    getSubmissionRate: vi.fn(),
    getRecipientQuota: vi.fn(),
    setRecipientQuota: vi.fn(),
    getPendingQuota: vi.fn(),
    setPendingQuota: vi.fn(),
    getAppSendingIp: vi.fn(),
    listSendingIps: vi.fn(),
    setSubmissionRate: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({
    showConfirm: vi.fn(),
    showConfirmDestroy: vi.fn(),
    showToast: vi.fn()
}));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { default: AppKeys } = await import('@/screens/app-keys.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const KEY_ID = 'fedcba9876543210fedcba9876543210';
const SECRET = 'tp_live_one_time_secret_value';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const activeKey = { id: KEY_ID, name: 'billing-worker', active: true, created_at: '2026-10-02T12:00:00Z' };

let wrapper: VueWrapper | undefined;

async function mountAt(path: string) {
    const mounted = await mountAppRoute(path, { 'app-keys': AppKeys });
    wrapper = mounted.wrapper;
    return wrapper;
}

function button(label: string) {
    const match = wrapper!.findAll('button').find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    localStorage.clear();
    sessionStorage.clear();
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.getSubmissionRate.mockResolvedValue({ data: { per_minute: 600, burst: 100 } });
    api.getRecipientQuota.mockResolvedValue({ data: { daily_limit: 1000, used: 120, day: '2026-11-19' } });
    api.getPendingQuota.mockResolvedValue({ data: { limit: 500, pending: 7, truncated: false } });
    api.getAppSendingIp.mockResolvedValue({ data: { sending_ip_id: null, address: null, helo_name: null } });
    api.listSendingIps.mockResolvedValue({ data: { ips: [], unpinned_app_count: 0, edge_reported_at: null, edge_reports_identity: false, edge_primary_address: null } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('app keys screen', () => {
    it('does not call the API for a malformed app ID', async () => {
        await mountAt('/apps/not-an-id');
        expect(wrapper!.text()).toContain('App not found');
        expect(api.listApps).not.toHaveBeenCalled();
        expect(api.listAppKeys).not.toHaveBeenCalled();
    });

    it('shows the issued secret once and never stores it', async () => {
        api.listAppKeys.mockResolvedValueOnce({ data: { keys: [] } }).mockResolvedValue({ data: { keys: [activeKey] } });
        api.issueAppKey.mockResolvedValue({ data: { key: activeKey, secret: SECRET } });
        vf.showConfirm.mockResolvedValue(true);
        const setItem = vi.spyOn(Storage.prototype, 'setItem');

        await mountAt(`/apps/${APP_ID}`);
        expect(wrapper!.text()).toContain('No API keys');

        await button('Create key').trigger('click');
        await wrapper!.find('input[type="text"]').setValue('billing-worker');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();

        expect(api.issueAppKey).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { name: 'billing-worker' } });
        const secretInput = wrapper!.find<HTMLInputElement>('input[readonly]');
        expect(secretInput.element.value).toBe(SECRET);
        expect(api.listAppKeys).toHaveBeenCalledTimes(2);
        expect(setItem.mock.calls.flat().join(' ')).not.toContain(SECRET);
        expect(JSON.stringify({ ...localStorage, ...sessionStorage })).not.toContain(SECRET);

        expect(button('Done').attributes('type')).toBe('submit');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();

        expect(vf.showConfirm).toHaveBeenCalled();
        expect(document.body.innerHTML).not.toContain(SECRET);
        expect(wrapper!.text()).toContain('billing-worker');
        setItem.mockRestore();
    });

    it('keeps the secret open when the user declines to close without copying', async () => {
        api.listAppKeys.mockResolvedValue({ data: { keys: [] } });
        api.issueAppKey.mockResolvedValue({ data: { key: activeKey, secret: SECRET } });
        vf.showConfirm.mockResolvedValue(false);

        await mountAt(`/apps/${APP_ID}`);
        await button('Create key').trigger('click');
        await wrapper!.find('input[type="text"]').setValue('billing-worker');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();

        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(wrapper!.find<HTMLInputElement>('input[readonly]').element.value).toBe(SECRET);
    });

    it('reports an uncertain outcome and shows no secret when issuing fails', async () => {
        api.listAppKeys.mockResolvedValue({ data: { keys: [] } });
        api.issueAppKey.mockRejectedValue(httpError(503));

        await mountAt(`/apps/${APP_ID}`);
        await button('Create key').trigger('click');
        await wrapper!.find('input[type="text"]').setValue('billing-worker');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();

        expect(wrapper!.text()).toContain("We can't tell whether the change went through");
        expect(wrapper!.find('input[readonly]').exists()).toBe(false);
    });

    it('rejects an invalid name without calling the API', async () => {
        api.listAppKeys.mockResolvedValue({ data: { keys: [] } });
        await mountAt(`/apps/${APP_ID}`);
        await button('Create key').trigger('click');
        await wrapper!.find('input[type="text"]').setValue(' padded');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.issueAppKey).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('Remove leading or trailing whitespace.');
    });

    it('revokes only after confirmation and reloads from the API', async () => {
        api.listAppKeys.mockResolvedValueOnce({ data: { keys: [activeKey] } }).mockResolvedValue({ data: { keys: [{ ...activeKey, active: false }] } });
        api.revokeAppKey.mockResolvedValue({ data: undefined });

        await mountAt(`/apps/${APP_ID}`);
        vf.showConfirmDestroy.mockResolvedValueOnce(false);
        await button('Revoke').trigger('click');
        await flushPromises();
        expect(api.revokeAppKey).not.toHaveBeenCalled();

        vf.showConfirmDestroy.mockResolvedValueOnce(true);
        await button('Revoke').trigger('click');
        await flushPromises();
        expect(api.revokeAppKey).toHaveBeenCalledWith({ path: { appId: APP_ID, keyId: KEY_ID } });
        expect(wrapper!.text()).toContain('Revoked');
        expect(wrapper!.findAll('button').some(b => b.text().startsWith('Revoke'))).toBe(false);
    });

    it('explains a failed revocation and reloads the current state', async () => {
        api.listAppKeys.mockResolvedValue({ data: { keys: [activeKey] } });
        api.revokeAppKey.mockRejectedValue(httpError(404));
        vf.showConfirmDestroy.mockResolvedValue(true);

        await mountAt(`/apps/${APP_ID}`);
        await button('Revoke').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('already revoked');
        expect(api.listAppKeys).toHaveBeenCalledTimes(2);
        expect(vf.showToast).not.toHaveBeenCalled();
    });

    it('does not offer key issuance for an inactive app', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        api.listAppKeys.mockResolvedValue({ data: { keys: [] } });
        await mountAt(`/apps/${APP_ID}`);
        expect(button('Create key').attributes('disabled')).toBeDefined();
        expect(wrapper!.text()).toContain("new API keys can't be created");
    });

    it('asks before leaving while a key is being issued and never shows a late secret', async () => {
        api.listAppKeys.mockResolvedValue({ data: { keys: [] } });
        let resolveIssue: (value: unknown) => void = () => undefined;
        api.issueAppKey.mockReturnValue(new Promise(resolve => (resolveIssue = resolve)));

        const mounted = await mountAt(`/apps/${APP_ID}`);
        const router = mounted.vm.$router;
        await button('Create key').trigger('click');
        await wrapper!.find('input[type="text"]').setValue('billing-worker');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();

        vf.showConfirm.mockResolvedValueOnce(false);
        await router.push('/apps');
        expect(router.currentRoute.value.path).toBe(`/apps/${APP_ID}`);

        vf.showConfirm.mockResolvedValueOnce(true);
        await router.push('/apps');
        await flushPromises();
        expect(router.currentRoute.value.path).toBe('/apps');

        resolveIssue({ data: { key: activeKey, secret: SECRET } });
        await flushPromises();
        expect(document.body.innerHTML).not.toContain(SECRET);
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: expect.stringContaining('Revoke it') }));
        expect(JSON.stringify(vf.showToast.mock.calls)).not.toContain(SECRET);
    });

    it('asks before switching app tabs while a secret is displayed', async () => {
        api.listAppKeys.mockResolvedValue({ data: { keys: [] } });
        api.issueAppKey.mockResolvedValue({ data: { key: activeKey, secret: SECRET } });

        const mounted = await mountAt(`/apps/${APP_ID}`);
        const router = mounted.vm.$router;
        await button('Create key').trigger('click');
        await wrapper!.find('input[type="text"]').setValue('billing-worker');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(wrapper!.find('nav[aria-label="App sections"] [aria-current="page"]').text()).toContain('API keys');

        vf.showConfirm.mockResolvedValueOnce(false);
        await router.push({ name: 'app-domains', params: { appId: APP_ID } });
        expect(router.currentRoute.value.name).toBe('app-keys');
        expect(wrapper!.find<HTMLInputElement>('input[readonly]').element.value).toBe(SECRET);

        vf.showConfirm.mockResolvedValueOnce(true);
        await router.push({ name: 'app-domains', params: { appId: APP_ID } });
        await flushPromises();
        expect(router.currentRoute.value.name).toBe('app-domains');
        expect(document.body.innerHTML).not.toContain(SECRET);
    });

    it('treats a malformed success response as an error, not an empty list', async () => {
        api.listAppKeys.mockResolvedValue({ data: '<!doctype html>' });
        await mountAt(`/apps/${APP_ID}`);
        expect(wrapper!.text()).toContain("something the console didn't understand");
        expect(wrapper!.text()).not.toContain('No API keys');
    });

    it('shows an authorization error instead of data on 403', async () => {
        api.listApps.mockRejectedValue(httpError(403));
        api.listAppKeys.mockRejectedValue(httpError(403));
        await mountAt(`/apps/${APP_ID}`);
        expect(wrapper!.text()).toContain("your account isn't allowed to use this console");
        expect(wrapper!.find('table').exists()).toBe(false);
    });
});
