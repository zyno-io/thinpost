import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Router } from 'vue-router';

import { httpError, networkError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppKeys: vi.fn(),
    issueAppKey: vi.fn(),
    revokeAppKey: vi.fn(),
    activateApp: vi.fn(),
    deactivateApp: vi.fn(),
    getPublicConfig: vi.fn(),
    getSubmissionRate: vi.fn(),
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
const activeApp = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const inactiveApp = { ...activeApp, active: false };
const noContent = () => Promise.resolve({ data: undefined, response: new Response(null, { status: 204 }) });

let wrapper: VueWrapper | undefined;
let router: Router;

async function mountAt(path: string) {
    const mounted = await mountAppRoute(path, { 'app-keys': AppKeys });
    router = mounted.router;
    wrapper = mounted.wrapper;
    return wrapper;
}

function statusPanel() {
    return wrapper!.find('section.app-status');
}

function panelButton(label: string) {
    const match = statusPanel()
        .findAll('button')
        .find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}

function hasPanelButton(label: string) {
    return statusPanel()
        .findAll('button')
        .some(b => b.text().includes(label));
}

function dialog() {
    return wrapper!.find('[role="alertdialog"]');
}

function dialogSubmit() {
    return dialog().find('button[type="submit"]');
}

async function openAction(label: 'Pause app' | 'Resume app') {
    await panelButton(label).trigger('click');
    await flushPromises();
    expect(dialog().exists()).toBe(true);
}

async function acknowledgeAndSubmit() {
    await dialog().find('input[type="checkbox"]').setValue(true);
    await wrapper!.find('form').trigger('submit');
    await flushPromises();
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    api.listApps.mockResolvedValue({ data: { apps: [activeApp] } });
    api.listAppKeys.mockResolvedValue({ data: { keys: [] } });
    api.getSubmissionRate.mockResolvedValue({ data: { per_minute: 600, burst: 100 } });
    api.getAppSendingIp.mockResolvedValue({ data: { sending_ip_id: null, name: null } });
    api.listSendingIps.mockResolvedValue({ data: { ips: [], unpinned_app_count: 0, edge_reported_at: null, edge_reports_sources: false, edge_primary_address: null, unnamed_edge_addresses: [] } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('app pause and resume', () => {
    it('shows the current status with the action that applies to it', async () => {
        await mountAt(`/apps/${APP_ID}`);
        expect(statusPanel().text()).toContain('App status');
        expect(statusPanel().text()).toContain('Active');
        expect(statusPanel().text()).toContain('As of');
        expect(hasPanelButton('Pause app')).toBe(true);
        expect(hasPanelButton('Resume app')).toBe(false);
    });

    it('explains what pausing does and does not stop, and requires acknowledgement', async () => {
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Pause app');

        const text = dialog().text();
        expect(text).toContain('Billing');
        expect(text).toContain(APP_ID);
        expect(text).toContain('new messages being accepted from this app');
        expect(text).toContain('queued messages being sent');
        // An attempt authorized before the pause can still start or finish after it.
        expect(text).toContain('Sends already under way may still finish');
        expect(text).toContain("they can't be recalled");
        expect(text).toContain("sends already under way may still finish, and pausing can't recall them");
        expect(text).not.toContain('permit issued before the pause');
        expect(text).toContain('API keys and queued messages are kept');
        expect(text).toContain('The pause is recorded in the audit log');

        expect(dialogSubmit().text()).toBe('Pause app');
        expect(dialogSubmit().attributes('disabled')).toBeDefined();
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.deactivateApp).not.toHaveBeenCalled();

        await dialog()
            .findAll('button')
            .find(b => b.text() === 'Cancel')!
            .trigger('click');
        await flushPromises();
        expect(dialog().exists()).toBe(false);
        expect(api.deactivateApp).not.toHaveBeenCalled();
    });

    it('pauses once, reloads the status and warns that previously authorized attempts may still start or finish', async () => {
        api.deactivateApp.mockImplementation(noContent);
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Pause app');
        api.listApps.mockResolvedValue({ data: { apps: [inactiveApp] } });
        await acknowledgeAndSubmit();

        expect(api.deactivateApp).toHaveBeenCalledTimes(1);
        expect(api.deactivateApp).toHaveBeenCalledWith({ path: { appId: APP_ID } });
        expect(api.activateApp).not.toHaveBeenCalled();
        expect(api.listApps).toHaveBeenCalledTimes(2);
        expect(dialog().exists()).toBe(false);

        const text = statusPanel().text();
        expect(text).toContain('Paused');
        expect(text).toContain('Sends already under way may still finish');
        const help = await openHelp(wrapper!, 'pausing');
        expect(help).toContain('A message that was already being sent when you paused can still go out');
        expect(help).toContain('a webhook event already on its way can still arrive');
        expect(help).toContain("Pausing can't recall them");
        expect(text).not.toContain('permit issued before the pause');
        expect(hasPanelButton('Resume app')).toBe(true);
        expect(wrapper!.find('.app-header .app-status').text()).toBe('Paused');
        const toast = vf.showToast.mock.calls[0]?.[0].message as string;
        expect(toast).toContain('Paused “Billing”');
        expect(toast).toContain('Sends already under way may still finish');
        // Key issuance follows the reloaded status.
        const issue = wrapper!.findAll('button').find(b => b.text().includes('Create key'))!;
        expect(issue.attributes('disabled')).toBeDefined();
    });

    it('shows a loading state and keeps the dialog open while the request is in flight', async () => {
        let resolve: (value: unknown) => void = () => undefined;
        api.deactivateApp.mockReturnValue(new Promise(r => (resolve = r)));
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Pause app');
        await acknowledgeAndSubmit();

        expect(dialogSubmit().text()).toBe('Pausing…');
        expect(dialogSubmit().attributes('disabled')).toBeDefined();
        const cancel = dialog()
            .findAll('button')
            .find(b => b.text() === 'Cancel')!;
        expect(cancel.attributes('disabled')).toBeDefined();
        expect(panelButton('Pausing…').attributes('disabled')).toBeDefined();
        expect(panelButton('Reload status').attributes('disabled')).toBeDefined();

        // A second submit does not send another request.
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.deactivateApp).toHaveBeenCalledTimes(1);

        api.listApps.mockResolvedValue({ data: { apps: [inactiveApp] } });
        resolve({ data: undefined, response: new Response(null, { status: 204 }) });
        await flushPromises();
        expect(dialog().exists()).toBe(false);
        expect(hasPanelButton('Resume app')).toBe(true);
    });

    it('explains before resuming that existing keys and retained queued work become eligible again', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [inactiveApp] } });
        api.activateApp.mockImplementation(noContent);
        await mountAt(`/apps/${APP_ID}`);
        expect(statusPanel().text()).toContain('Paused');
        expect(statusPanel().text()).toContain('API keys and queued messages are kept');

        await openAction('Resume app');
        const text = dialog().text();
        expect(text).toContain("API keys that haven't been revoked work again, and queued messages can be sent");
        expect(text).toContain('Sending limits, recipient rules and domain verification still apply');
        expect(dialogSubmit().text()).toBe('Resume app');
        expect(dialogSubmit().attributes('disabled')).toBeDefined();

        api.listApps.mockResolvedValue({ data: { apps: [activeApp] } });
        await acknowledgeAndSubmit();

        expect(api.activateApp).toHaveBeenCalledTimes(1);
        expect(api.activateApp).toHaveBeenCalledWith({ path: { appId: APP_ID } });
        expect(api.deactivateApp).not.toHaveBeenCalled();
        expect(hasPanelButton('Pause app')).toBe(true);
        const toast = vf.showToast.mock.calls[0]?.[0].message as string;
        expect(toast).toContain('Resumed “Billing”');
        expect(toast).toContain('subject to other limits');
        expect(statusPanel().text()).not.toContain('Sends already under way');
    });

    it.each([
        ['no response', () => Promise.reject(networkError()), "Couldn't reach Thinpost"],
        ['a 503', () => Promise.reject(httpError(503)), 'Pausing is temporarily unavailable'],
        ['a 502', () => Promise.reject(httpError(502)), 'Thinpost is temporarily unavailable'],
        ['a success other than 204', () => Promise.resolve({ data: '<html>', response: new Response('<html>', { status: 200 }) }), "something the console didn't understand"]
    ])('requires a successful reload before retrying after %s', async (_label, result, message) => {
        api.deactivateApp.mockImplementation(result);
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Pause app');
        await acknowledgeAndSubmit();

        const text = statusPanel().text();
        expect(text).toContain(message);
        expect(text).toContain('may or may not have gone through');
        expect(text).toContain('Reload the status first');
        expect(vf.showToast).not.toHaveBeenCalled();
        // No automatic replay and no automatic reload.
        expect(api.deactivateApp).toHaveBeenCalledTimes(1);
        expect(api.listApps).toHaveBeenCalledTimes(1);
        expect(panelButton('Pause app').attributes('disabled')).toBeDefined();
        await panelButton('Pause app').trigger('click');
        await flushPromises();
        expect(dialog().exists()).toBe(false);

        // A failed reload keeps the block.
        api.listApps.mockRejectedValueOnce(networkError());
        await panelButton('Reload status').trigger('click');
        await flushPromises();
        expect(statusPanel().text()).toContain('from the last time it loaded');
        expect(statusPanel().text()).toContain("can't change the status until a reload succeeds");
        expect(panelButton('Pause app').attributes('disabled')).toBeDefined();

        // A successful reload shows the actual state and unblocks.
        api.listApps.mockResolvedValue({ data: { apps: [inactiveApp] } });
        await panelButton('Reload status').trigger('click');
        await flushPromises();
        expect(api.listApps).toHaveBeenCalledTimes(3);
        expect(statusPanel().text()).not.toContain('may or may not have gone through');
        expect(statusPanel().text()).not.toContain('Reload the status first');
        expect(panelButton('Resume app').attributes('disabled')).toBeUndefined();
        expect(api.deactivateApp).toHaveBeenCalledTimes(1);
    });

    it('offers the reload from the error notice without replaying the request', async () => {
        api.activateApp.mockRejectedValue(networkError());
        api.listApps.mockResolvedValue({ data: { apps: [inactiveApp] } });
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Resume app');
        await acknowledgeAndSubmit();

        const notice = statusPanel().find('.api-error');
        expect(notice.text()).toContain("We can't tell whether the change went through");
        const reload = notice.find('button');
        expect(reload.text()).toBe('Reload status');
        api.listApps.mockResolvedValue({ data: { apps: [activeApp] } });
        await reload.trigger('click');
        await flushPromises();

        expect(api.activateApp).toHaveBeenCalledTimes(1);
        expect(statusPanel().find('.api-error').exists()).toBe(false);
        expect(hasPanelButton('Pause app')).toBe(true);
    });

    it('handles session loss: nothing changed, no reload, and actions blocked until sign-in', async () => {
        api.deactivateApp.mockImplementation(() => {
            // The client's 401 handler ends the session before the error reaches the caller.
            authState.status = 'ended';
            return Promise.reject(httpError(401));
        });
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Pause app');
        await acknowledgeAndSubmit();

        const text = statusPanel().text();
        expect(text).toContain('Your session has ended');
        expect(text).toContain('Nothing was changed');
        expect(text).not.toContain('may or may not have gone through');
        expect(text).toContain('Sign in again to change the app status');
        expect(api.listApps).toHaveBeenCalledTimes(1);
        expect(panelButton('Pause app').attributes('disabled')).toBeDefined();
        expect(panelButton('Reload status').attributes('disabled')).toBeDefined();
        expect(statusPanel().find('.api-error button').exists()).toBe(false);
    });

    it('does not send the request when the session ends while the dialog is open', async () => {
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Pause app');
        authState.status = 'ended';
        await flushPromises();

        expect(dialog().text()).toContain('Sign in again to change the app status');
        expect(dialogSubmit().attributes('disabled')).toBeDefined();
        await acknowledgeAndSubmit();
        expect(api.deactivateApp).not.toHaveBeenCalled();
    });

    it('reports a success whose status reload is blocked by an ended session', async () => {
        api.deactivateApp.mockImplementation(() => {
            authState.status = 'ended';
            return noContent();
        });
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Pause app');
        await acknowledgeAndSubmit();

        expect(api.listApps).toHaveBeenCalledTimes(1);
        expect(statusPanel().text()).toContain('The pause request worked, but your session ended');
        expect(wrapper!.find('.app-header .app-status').text()).toBe('Paused');
    });

    it.each([
        [404, 'The app was not found. Nothing was paused.'],
        [409, "Thinpost couldn't pause the app because its state just changed. Nothing was changed."]
    ])('treats a %i as a definitive refusal and reloads the status', async (status, message) => {
        api.deactivateApp.mockRejectedValue(httpError(status));
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Pause app');
        if (status === 404) api.listApps.mockResolvedValue({ data: { apps: [] } });
        await acknowledgeAndSubmit();

        const text = statusPanel().text();
        expect(text).toContain(message);
        expect(text).not.toContain('may or may not have gone through');
        expect(api.listApps).toHaveBeenCalledTimes(2);
        expect(api.deactivateApp).toHaveBeenCalledTimes(1);
        if (status === 404) {
            expect(text).toContain('The app was not found the last time its status was loaded');
            expect(panelButton('Pause app').attributes('disabled')).toBeDefined();
        } else {
            expect(panelButton('Pause app').attributes('disabled')).toBeUndefined();
        }
    });

    it('reports a definitive 403 without blocking or reloading', async () => {
        api.activateApp.mockRejectedValue(httpError(403));
        api.listApps.mockResolvedValue({ data: { apps: [inactiveApp] } });
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Resume app');
        await acknowledgeAndSubmit();

        expect(statusPanel().text()).toContain("your account isn't allowed to use this console");
        expect(statusPanel().text()).toContain('Nothing was changed');
        expect(api.listApps).toHaveBeenCalledTimes(1);
        expect(panelButton('Resume app').attributes('disabled')).toBeUndefined();
    });

    it('flags a reloaded status that differs from the confirmed change', async () => {
        api.deactivateApp.mockImplementation(noContent);
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Pause app');
        await acknowledgeAndSubmit();

        expect(statusPanel().text()).toContain('The pause request worked, but the app now shows as active');
        expect(hasPanelButton('Pause app')).toBe(true);
        expect(statusPanel().text()).not.toContain('Sends already under way');
    });

    it('shows the confirmed status when the reload after a success fails', async () => {
        api.deactivateApp.mockImplementation(noContent);
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Pause app');
        api.listApps.mockRejectedValue(httpError(503));
        await acknowledgeAndSubmit();

        expect(statusPanel().text()).toContain("The pause request worked, but the status couldn't be reloaded");
        expect(wrapper!.find('.app-header .app-status').text()).toBe('Paused');
        expect(statusPanel().text()).toContain('Sends already under way may still finish');
        expect(panelButton('Resume app').attributes('disabled')).toBeUndefined();
    });

    it('asks before leaving mid-request and reports the outcome without request details', async () => {
        let resolve: (value: unknown) => void = () => undefined;
        api.deactivateApp.mockReturnValue(new Promise(r => (resolve = r)));
        await mountAt(`/apps/${APP_ID}`);
        await openAction('Pause app');
        await acknowledgeAndSubmit();

        vf.showConfirm.mockResolvedValueOnce(false);
        await router.push('/apps');
        await flushPromises();
        expect(router.currentRoute.value.path).toBe(`/apps/${APP_ID}`);
        expect(vf.showConfirm.mock.calls[0]?.[0]).toContain('app status is changing');

        vf.showConfirm.mockResolvedValue(true);
        await router.push('/apps');
        await flushPromises();
        expect(router.currentRoute.value.path).toBe('/apps');

        resolve(Promise.reject(networkError()));
        await flushPromises();
        expect(api.deactivateApp).toHaveBeenCalledTimes(1);
        const toast = vf.showToast.mock.calls.at(-1)?.[0].message as string;
        expect(toast).toContain('may or may not have gone through');
        expect(toast).not.toContain(APP_ID);
    });
});
