import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Router } from 'vue-router';

import { httpError } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppKeys: vi.fn(),
    renameApp: vi.fn(),
    deleteApp: vi.fn(),
    activateApp: vi.fn(),
    deactivateApp: vi.fn(),
    getPublicConfig: vi.fn(),
    getSubmissionRate: vi.fn(),
    getRecipientQuota: vi.fn(),
    getPendingQuota: vi.fn(),
    getAppSendingIp: vi.fn(),
    listSendingIps: vi.fn()
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
const { rememberApp, lastAppId } = await import('@/last-app');

const APP_ID = '0123456789abcdef0123456789abcdef';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z', sending_ip_id: null };
const noContent = () => Promise.resolve({ data: undefined, response: new Response(null, { status: 204 }) });

let wrapper: VueWrapper | undefined;
let router: Router;

async function mountApp() {
    const mounted = await mountAppRoute(`/apps/${APP_ID}`, { 'app-keys': AppKeys });
    router = mounted.router;
    wrapper = mounted.wrapper;
    return wrapper;
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.listAppKeys.mockResolvedValue({ data: { keys: [] } });
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

async function openRename() {
    await wrapper!.find('[data-testid="rename-app"]').trigger('click');
    await flushPromises();
    const dialog = wrapper!.find('[role="dialog"]');
    expect(dialog.exists()).toBe(true);
    return dialog;
}

async function openDelete() {
    await wrapper!.find('[data-testid="delete-app"]').trigger('click');
    await flushPromises();
    const dialog = wrapper!.find('[role="alertdialog"][data-delete-app]');
    expect(dialog.exists()).toBe(true);
    return dialog;
}

describe('rename app', () => {
    it('opens with the current name, renames and shows the new name', async () => {
        await mountApp();
        api.renameApp.mockResolvedValue({ data: { ...app, name: 'Invoicing' } });
        const dialog = await openRename();
        const input = dialog.find('input[type="text"]');
        expect((input.element as HTMLInputElement).value).toBe('Billing');
        await input.setValue('Invoicing');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.renameApp).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { name: 'Invoicing' } });
        expect(wrapper!.find('h1').text()).toBe('Invoicing');
        expect(wrapper!.find('[role="dialog"]').exists()).toBe(false);
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Renamed to “Invoicing”' }));
    });

    it('does not send a blank, padded or unchanged name', async () => {
        await mountApp();
        const dialog = await openRename();
        const input = dialog.find('input[type="text"]');
        await input.setValue('');
        await wrapper!.find('form').trigger('submit');
        await input.setValue(' padded');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.renameApp).not.toHaveBeenCalled();
        await input.setValue('Billing');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.renameApp).not.toHaveBeenCalled();
        expect(wrapper!.find('[role="dialog"]').exists()).toBe(false);
    });

    it('keeps the dialog open and explains a refusal', async () => {
        await mountApp();
        api.renameApp.mockRejectedValue(httpError(404));
        const dialog = await openRename();
        await dialog.find('input[type="text"]').setValue('Invoicing');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(wrapper!.find('[role="dialog"]').text()).toContain('may have been deleted');
        expect(wrapper!.find('h1').text()).toBe('Billing');
    });
});

describe('rename targets the app it was opened for', () => {
    const OTHER_ID = 'fedcba9876543210fedcba9876543210';
    const other = { ...app, id: OTHER_ID, name: 'Shipping' };

    it('offers no rename while the app is still loading', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [app, other] } });
        await mountApp();
        expect(wrapper!.find('[data-testid="rename-app"]').exists()).toBe(true);
        // The next app is known from the list, but its load has not finished.
        api.listApps.mockReturnValue(new Promise(() => {}));
        await router.push(`/apps/${OTHER_ID}`);
        await flushPromises();
        expect(wrapper!.find('h1').text()).toBe('Shipping');
        expect(wrapper!.find('[data-testid="rename-app"]').exists()).toBe(false);
    });

    it('closes the dialog when the route moves to another app', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [app, other] } });
        await mountApp();
        await openRename();
        await router.push(`/apps/${OTHER_ID}`);
        await flushPromises();
        expect(wrapper!.find('[role="dialog"]').exists()).toBe(false);
        expect(api.renameApp).not.toHaveBeenCalled();
    });

    it('sends the rename for the app the dialog was opened for', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [app, other] } });
        await mountApp();
        let finish!: (value: unknown) => void;
        api.renameApp.mockReturnValue(new Promise(resolve => (finish = resolve)));
        const dialog = await openRename();
        await dialog.find('input[type="text"]').setValue('Invoicing');
        await wrapper!.find('form').trigger('submit');
        await router.push(`/apps/${OTHER_ID}`);
        await flushPromises();
        finish({ data: { ...app, name: 'Invoicing' } });
        await flushPromises();
        expect(api.renameApp).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { name: 'Invoicing' } });
        expect(wrapper!.find('h1').text()).toBe('Shipping');
    });

    it('a finished rename does not close or toast over another app\'s dialog', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [app, other] } });
        await mountApp();
        let finish!: (value: unknown) => void;
        api.renameApp.mockReturnValueOnce(new Promise(resolve => (finish = resolve)));
        const first = await openRename();
        await first.find('input[type="text"]').setValue('Invoicing');
        await wrapper!.find('form').trigger('submit');
        await router.push(`/apps/${OTHER_ID}`);
        await flushPromises();
        // A dialog for the other app, opened while the first request is still out.
        const second = await openRename();
        expect((second.find('input[type="text"]').element as HTMLInputElement).value).toBe('Shipping');
        expect(second.find('button[type="submit"]').attributes('disabled')).toBeUndefined();
        finish({ data: { ...app, name: 'Invoicing' } });
        await flushPromises();
        expect(wrapper!.find('[role="dialog"]').exists()).toBe(true);
        expect(vf.showToast).not.toHaveBeenCalled();
        expect(wrapper!.find('h1').text()).toBe('Shipping');
    });
});

describe('delete app', () => {
    it('requires the app name before the button works', async () => {
        await mountApp();
        const dialog = await openDelete();
        expect(dialog.text()).toContain('API keys and SMTP credentials stop working');
        expect(dialog.text()).toContain('kept, not erased');
        const submit = dialog.find('button[type="submit"]');
        expect(submit.attributes('disabled')).toBeDefined();
        const input = dialog.find('input[type="text"]');
        await input.setValue('billing');
        expect(submit.attributes('disabled')).toBeDefined();
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.deleteApp).not.toHaveBeenCalled();
        await input.setValue('Billing');
        expect(submit.attributes('disabled')).toBeUndefined();
    });

    it('deletes, forgets the app, shows a toast and returns to the apps list', async () => {
        await mountApp();
        rememberApp(APP_ID);
        api.deleteApp.mockImplementation(noContent);
        const dialog = await openDelete();
        await dialog.find('input[type="text"]').setValue('Billing');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.deleteApp).toHaveBeenCalledWith({ path: { appId: APP_ID } });
        expect(router.currentRoute.value.name).toBe('apps');
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: expect.stringContaining('Deleted “Billing”') }));
        expect(lastAppId.value).toBeNull();
    });

    it('stays on the app and explains a failure', async () => {
        await mountApp();
        api.deleteApp.mockRejectedValue(httpError(404));
        const dialog = await openDelete();
        await dialog.find('input[type="text"]').setValue('Billing');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(router.currentRoute.value.path).toBe(`/apps/${APP_ID}`);
        expect(wrapper!.find('[role="alertdialog"][data-delete-app]').text()).toContain('already have been deleted');
        expect(vf.showToast).not.toHaveBeenCalled();
    });

    it('warns that the outcome is unknown when no answer arrives', async () => {
        await mountApp();
        api.deleteApp.mockRejectedValue(new TypeError('Failed to fetch'));
        const dialog = await openDelete();
        await dialog.find('input[type="text"]').setValue('Billing');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(router.currentRoute.value.path).toBe(`/apps/${APP_ID}`);
        expect(wrapper!.find('[role="alertdialog"][data-delete-app]').exists()).toBe(true);
    });

    it('cancel leaves the app alone', async () => {
        await mountApp();
        const dialog = await openDelete();
        const cancel = dialog.findAll('button').find(b => b.text() === 'Cancel')!;
        await cancel.trigger('click');
        await flushPromises();
        expect(wrapper!.find('[role="alertdialog"][data-delete-app]').exists()).toBe(false);
        expect(api.deleteApp).not.toHaveBeenCalled();
    });
});
