import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { Router } from 'vue-router';

import { httpError, networkError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppWebhooks: vi.fn(),
    createAppWebhook: vi.fn(),
    updateAppWebhook: vi.fn(),
    deleteAppWebhook: vi.fn(),
    rotateAppWebhookSecret: vi.fn(),
    testAppWebhook: vi.fn(),
    listWebhookDeliveries: vi.fn(),
    listWebhookDeliveryAttempts: vi.fn(),
    replayWebhookDelivery: vi.fn(),
    getPublicConfig: vi.fn()
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

const { authState, signOut } = await import('@/auth');
const { default: AppWebhooks } = await import('@/screens/app-webhooks.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const ID_ENABLED = '11111111111111111111111111111111';
const ID_NEW = '22222222222222222222222222222222';
const ID_DISABLED = '33333333333333333333333333333333';
const SECRET = 'whsec_one_time_secret_value';
const NEW_ENDPOINT = 'https://hooks.example.com/new';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };

function hook(id: string, endpoint: string, active: boolean, extra: Record<string, unknown> = {}) {
    return { id, format: 'thinpost', endpoint, description: '', active, created_at: '2026-10-02T12:00:00Z', secret_created_at: '2026-10-03T12:00:00Z', ...extra };
}
const enabledHook = hook(ID_ENABLED, 'https://hooks.example.com/enabled', true, { description: 'Billing receiver' });
const disabledHook = hook(ID_DISABLED, 'https://hooks.example.com/disabled', false, { events: ['accepted', 'rejected'] });
const newHook = hook(ID_NEW, NEW_ENDPOINT, true);
const webhooks = [enabledHook, disabledHook];

let wrapper: VueWrapper | undefined;
let router: Router | undefined;

async function mountAt(path: string) {
    const mounted = await mountAppRoute(path, { 'app-webhooks': AppWebhooks });
    wrapper = mounted.wrapper;
    router = mounted.router;
    return wrapper;
}

function rowFor(id: string) {
    const match = wrapper!.findAll('tbody tr').find(r => r.text().includes(id));
    if (!match) throw new Error(`row for ${id} not found`);
    return match;
}

function buttonIn(scope: ReturnType<typeof rowFor> | VueWrapper, label: string) {
    const match = scope.findAll('button').find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}

/** Opens the row's overflow menu and clicks one of its items. */
async function menuAction(id: string, label: string) {
    await rowFor(id).find('.dropdown-trigger').trigger('click');
    await flushPromises();
    await buttonIn(rowFor(id), label).trigger('click');
    await flushPromises();
}

function lists(w: unknown = webhooks) {
    api.listAppWebhooks.mockResolvedValue({ data: { webhooks: w } });
}

async function submitEndpoint(url: string, description?: string) {
    await buttonIn(wrapper!, 'Add webhook').trigger('click');
    await wrapper!.find('input[inputmode="url"]').setValue(url);
    if (description !== undefined) await wrapper!.find('input[id$="-description"]').setValue(description);
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
    // Delivery history is covered in webhook-delivery-history.spec.ts.
    api.listWebhookDeliveries.mockResolvedValue({ data: { deliveries: [] } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('webhooks screen', () => {
    it('does not call the API for a malformed app ID', async () => {
        await mountAt('/apps/nope/webhooks');
        expect(wrapper!.text()).toContain('App not found');
        expect(api.listAppWebhooks).not.toHaveBeenCalled();
    });

    it('lists endpoints with description, event filter, status and secret issue date', async () => {
        lists();
        await mountAt(`/apps/${APP_ID}/webhooks`);
        expect(api.listAppWebhooks).toHaveBeenCalledWith({ path: { appId: APP_ID } });
        expect(wrapper!.find('#webhooks-heading').text()).toBe('Webhooks');

        const enabled = rowFor(ID_ENABLED);
        expect(enabled.text()).toContain('https://hooks.example.com/enabled');
        expect(enabled.text()).toContain('Billing receiver');
        expect(enabled.text()).toContain('All events');
        expect(enabled.text()).toContain('Enabled');
        expect(enabled.text()).toContain('Created');
        expect(enabled.findAll('button').map(b => b.text())).toEqual(
            expect.arrayContaining([expect.stringContaining('Send test'), expect.stringContaining('Disable')])
        );

        const disabled = rowFor(ID_DISABLED);
        expect(disabled.text()).toContain('2 event types');
        expect(disabled.find('.events').attributes('title')).toBe('accepted, rejected');
        expect(disabled.text()).toContain('Disabled');
        expect(buttonIn(disabled, 'Enable').attributes('disabled')).toBeUndefined();
    });

    it('allows one mutation at a time', async () => {
        lists();
        let resolveTest!: (value: unknown) => void;
        api.testAppWebhook.mockReturnValue(new Promise(resolve => (resolveTest = resolve)));
        await mountAt(`/apps/${APP_ID}/webhooks`);

        await buttonIn(rowFor(ID_ENABLED), 'Send test').trigger('click');
        await flushPromises();
        expect(buttonIn(rowFor(ID_ENABLED), 'Sending…').attributes('disabled')).toBeDefined();
        expect(buttonIn(rowFor(ID_DISABLED), 'Enable').attributes('disabled')).toBeDefined();
        expect(buttonIn(rowFor(ID_ENABLED), 'Disable').attributes('disabled')).toBeDefined();
        expect(buttonIn(wrapper!, 'Add webhook').attributes('disabled')).toBeDefined();
        await buttonIn(rowFor(ID_DISABLED), 'Enable').trigger('click');
        await buttonIn(rowFor(ID_ENABLED), 'Disable').trigger('click');
        await flushPromises();
        expect(api.testAppWebhook).toHaveBeenCalledTimes(1);
        expect(api.updateAppWebhook).not.toHaveBeenCalled();
        expect(vf.showConfirmDestroy).not.toHaveBeenCalled();

        resolveTest({ data: { success: true, http_status: 204 } });
        await flushPromises();
        expect(buttonIn(rowFor(ID_DISABLED), 'Enable').attributes('disabled')).toBeUndefined();
    });

    it('sends no disable request when the page is left or the session ends during confirmation', async () => {
        lists();
        let answer!: (value: boolean) => void;
        vf.showConfirmDestroy.mockImplementation(() => new Promise(resolve => (answer = resolve)));

        await mountAt(`/apps/${APP_ID}/webhooks`);
        await buttonIn(rowFor(ID_ENABLED), 'Disable').trigger('click');
        authState.status = 'ended';
        answer(true);
        await flushPromises();
        expect(api.updateAppWebhook).not.toHaveBeenCalled();

        authState.status = 'signed-in';
        await flushPromises();
        await buttonIn(rowFor(ID_ENABLED), 'Disable').trigger('click');
        wrapper!.unmount();
        wrapper = undefined;
        answer(true);
        await flushPromises();
        expect(api.updateAppWebhook).not.toHaveBeenCalled();
    });

    it('reports a failed disable without a success toast and reloads the current state', async () => {
        lists();
        api.updateAppWebhook.mockRejectedValue(httpError(503));
        vf.showConfirmDestroy.mockResolvedValue(true);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await buttonIn(rowFor(ID_ENABLED), 'Disable').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('Webhooks are unavailable right now.');
        expect(wrapper!.text()).toContain("can't tell whether the change went through");
        expect(vf.showToast).not.toHaveBeenCalled();
        expect(api.listAppWebhooks).toHaveBeenCalledTimes(2);
    });

    it('ignores a late response for an app the page has navigated away from', async () => {
        const OTHER_ID = 'abcdefabcdefabcdefabcdefabcdefab';
        api.listApps.mockResolvedValue({ data: { apps: [app, { ...app, id: OTHER_ID, name: 'Support' }] } });
        let resolveFirst!: (value: unknown) => void;
        api.listAppWebhooks
            .mockReturnValueOnce(new Promise(resolve => (resolveFirst = resolve)))
            .mockResolvedValue({ data: { webhooks: [hook(ID_NEW, 'https://support.example.com/hook', true)] } });

        await mountAt(`/apps/${APP_ID}/webhooks`);
        const router = wrapper!.vm.$router;
        await router.push(`/apps/${OTHER_ID}/webhooks`);
        await flushPromises();
        resolveFirst({ data: { webhooks } });
        await flushPromises();

        expect(wrapper!.text()).toContain('Support');
        expect(wrapper!.text()).toContain('https://support.example.com/hook');
        expect(wrapper!.text()).not.toContain('https://hooks.example.com/enabled');
    });

    it('explains the webhook contract without any approval or catalog language', async () => {
        lists();
        await mountAt(`/apps/${APP_ID}/webhooks`);
        expect(wrapper!.text()).not.toContain('Thinpost sends JSON events');
        const text = await openHelp(wrapper!, 'webhooks');
        expect(text).toContain('Thinpost sends JSON events');
        expect(text).toContain('shown once');
        expect(text).toContain('X-Thinpost-Event-ID');
        expect(text).toContain('port 443');
        expect(text).toContain('ignore repeats using');
        expect(text).toContain('An event already on its way may still arrive');
        expect(text).toContain('X-PM-Webhook-Trace-Id');
        expect(text).toContain('SendGrid-style webhooks');
        expect(wrapper!.text()).toContain('Webhook event history');
        expect(text).not.toContain('approved by the operator');
        expect(text).not.toMatch(/approv|catalog|operator-managed/i);
    });

    it('renders endpoints as text and never displays secret material from a list', async () => {
        lists([hook(ID_NEW, 'https://hooks.example.com/<img src=x onerror=alert(1)>', true, { secret: 'whsec_hidden' })]);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        const row = rowFor(ID_NEW);
        expect(row.find('img').exists()).toBe(false);
        expect(row.text()).toContain('<img src=x onerror=alert(1)>');
        expect(row.find('a').exists()).toBe(false);
        expect(wrapper!.html()).not.toContain('whsec_hidden');
    });

    it('re-enables only after no confirmation and disables only after confirmation that mentions in-flight delivery', async () => {
        lists();
        api.updateAppWebhook.mockResolvedValue({ data: { ...disabledHook, active: true } });
        await mountAt(`/apps/${APP_ID}/webhooks`);

        await buttonIn(rowFor(ID_DISABLED), 'Enable').trigger('click');
        await flushPromises();
        expect(vf.showConfirmDestroy).not.toHaveBeenCalled();
        expect(api.updateAppWebhook).toHaveBeenCalledWith({ path: { appId: APP_ID, webhookId: ID_DISABLED }, body: { active: true } });
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Webhook enabled' }));
        expect(api.listAppWebhooks).toHaveBeenCalledTimes(2);

        api.updateAppWebhook.mockClear();
        vf.showConfirmDestroy.mockResolvedValueOnce(false);
        await buttonIn(rowFor(ID_ENABLED), 'Disable').trigger('click');
        await flushPromises();
        expect(vf.showConfirmDestroy).toHaveBeenCalledWith(
            'Disable webhook',
            expect.stringMatching(/https:\/\/hooks\.example\.com\/enabled.*already on its way may still arrive/)
        );
        expect(api.updateAppWebhook).not.toHaveBeenCalled();

        vf.showConfirmDestroy.mockResolvedValueOnce(true);
        api.updateAppWebhook.mockResolvedValue({ data: { ...enabledHook, active: false } });
        lists([{ ...enabledHook, active: false }, disabledHook]);
        await buttonIn(rowFor(ID_ENABLED), 'Disable').trigger('click');
        await flushPromises();
        expect(api.updateAppWebhook).toHaveBeenCalledWith({ path: { appId: APP_ID, webhookId: ID_ENABLED }, body: { active: false } });
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: expect.stringContaining('already on its way may still arrive') }));
        expect(rowFor(ID_ENABLED).text()).toContain('Disabled');
    });

    it('reports webhook administration as unavailable on 503 instead of showing an empty list', async () => {
        api.listAppWebhooks.mockRejectedValue(httpError(503));
        await mountAt(`/apps/${APP_ID}/webhooks`);
        const text = wrapper!.text();
        expect(text).toContain('Billing');
        expect(text).toContain('Webhooks are unavailable right now.');
        expect(text).toContain('ask your Thinpost administrator');
        expect(text).not.toContain('No webhooks yet');
        expect(wrapper!.find('table').exists()).toBe(false);

        lists([]);
        const retry = wrapper!.findAll('button').find(b => b.text() === 'Try again')!;
        await retry.trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('No webhooks yet');
    });

    it('treats a 404 from the list as a missing app', async () => {
        api.listAppWebhooks.mockRejectedValue(httpError(404));
        await mountAt(`/apps/${APP_ID}/webhooks`);
        expect(wrapper!.text()).toContain('The app was not found.');
    });

    it('explains a 404 from enabling as a deleted endpoint or inactive app', async () => {
        lists();
        api.updateAppWebhook.mockRejectedValue(httpError(404));
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await buttonIn(rowFor(ID_DISABLED), 'Enable').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("webhook wasn't found, or the app is paused");
        expect(api.listAppWebhooks).toHaveBeenCalledTimes(2);
    });

    it('explains a 409 from enabling as the ten-endpoint limit', async () => {
        lists();
        api.updateAppWebhook.mockRejectedValue(httpError(409));
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await buttonIn(rowFor(ID_DISABLED), 'Enable').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('10 webhooks are already enabled. Disable one first.');
    });

    it('rejects an enable response that does not report the endpoint as active', async () => {
        lists();
        api.updateAppWebhook.mockResolvedValue({ data: { ...disabledHook, active: false } });
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await buttonIn(rowFor(ID_DISABLED), 'Enable').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("didn't understand");
        expect(vf.showToast).not.toHaveBeenCalled();
    });

    it('does not offer adding or enabling for an inactive app but still allows disabling', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        lists();
        await mountAt(`/apps/${APP_ID}/webhooks`);
        expect(wrapper!.text()).toContain("webhooks can't be added or enabled");
        expect(buttonIn(wrapper!, 'Add webhook').attributes('disabled')).toBeDefined();
        expect(buttonIn(rowFor(ID_DISABLED), 'Enable').attributes('disabled')).toBeDefined();
        expect(buttonIn(rowFor(ID_ENABLED), 'Disable').attributes('disabled')).toBeUndefined();
    });

    it('disables mutations once the session has ended', async () => {
        lists();
        await mountAt(`/apps/${APP_ID}/webhooks`);
        authState.status = 'ended';
        await flushPromises();
        expect(buttonIn(wrapper!, 'Add webhook').attributes('disabled')).toBeDefined();
        expect(buttonIn(rowFor(ID_DISABLED), 'Enable').attributes('disabled')).toBeDefined();
        expect(buttonIn(rowFor(ID_ENABLED), 'Disable').attributes('disabled')).toBeDefined();
        expect(buttonIn(rowFor(ID_ENABLED), 'Send test').attributes('disabled')).toBeDefined();
    });

    it('shows an error instead of a list for a malformed response', async () => {
        lists([{ id: ID_ENABLED, endpoint: 'https://hooks.example.com/enabled' }]);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        expect(wrapper!.text()).toContain("didn't understand");
        expect(wrapper!.find('table').exists()).toBe(false);
    });

    it('shows an empty state when there are no endpoints', async () => {
        lists([]);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        expect(wrapper!.text()).toContain('No webhooks yet');
        expect(wrapper!.find('table').exists()).toBe(false);
    });
});

describe('adding an endpoint', () => {
    it('creates an endpoint, shows the secret once and clears it after Done', async () => {
        api.listAppWebhooks.mockResolvedValueOnce({ data: { webhooks: [] } }).mockResolvedValue({ data: { webhooks: [newHook] } });
        api.createAppWebhook.mockResolvedValue({ data: { webhook: newHook, secret: SECRET } });
        vf.showConfirm.mockResolvedValue(true);
        const setItem = vi.spyOn(Storage.prototype, 'setItem');
        await mountAt(`/apps/${APP_ID}/webhooks`);

        await submitEndpoint(NEW_ENDPOINT, 'New receiver');
        expect(api.createAppWebhook).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { format: 'thinpost', endpoint: NEW_ENDPOINT, description: 'New receiver' } });
        expect(wrapper!.find<HTMLInputElement>('input[readonly]').element.value).toBe(SECRET);
        expect(wrapper!.text()).toContain('Use this exact text as the HMAC-SHA256 key to check signatures.');
        expect(api.listAppWebhooks).toHaveBeenCalledTimes(2);
        expect(setItem.mock.calls.flat().join(' ')).not.toContain(SECRET);
        expect(JSON.stringify({ ...localStorage, ...sessionStorage })).not.toContain(SECRET);
        expect(router!.currentRoute.value.fullPath).not.toContain(SECRET);
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Webhook added' }));

        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(document.body.innerHTML).not.toContain(SECRET);
        expect(wrapper!.html()).not.toContain(SECRET);
        expect(wrapper!.text()).toContain(NEW_ENDPOINT);
        setItem.mockRestore();
    });

    it('sends the selected event types and omits events for all events', async () => {
        lists([]);
        api.createAppWebhook.mockResolvedValue({ data: { webhook: newHook, secret: SECRET } });
        await mountAt(`/apps/${APP_ID}/webhooks`);

        await buttonIn(wrapper!, 'Add webhook').trigger('click');
        await wrapper!.find('input[inputmode="url"]').setValue(NEW_ENDPOINT);
        await wrapper!.find('input[type="radio"][value="selected"]').setValue(true);
        await wrapper!.find('input[type="checkbox"][value="clicked"]').setValue(true);
        await wrapper!.find('input[type="checkbox"][value="accepted"]').setValue(true);
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.createAppWebhook).toHaveBeenCalledWith({
            path: { appId: APP_ID },
            body: { format: 'thinpost', endpoint: NEW_ENDPOINT, description: '', events: ['accepted', 'clicked'] }
        });
    });

    it('rejects invalid input without calling the API', async () => {
        lists([]);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await submitEndpoint('http://hooks.example.com/x');
        expect(api.createAppWebhook).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('Use an https:// address.');

        await wrapper!.find('input[inputmode="url"]').setValue(NEW_ENDPOINT);
        await wrapper!.find('input[type="radio"][value="selected"]').setValue(true);
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.createAppWebhook).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('Select at least one event type');
    });

    it('asks before leaving while the secret is displayed', async () => {
        lists([]);
        api.createAppWebhook.mockResolvedValue({ data: { webhook: newHook, secret: SECRET } });
        const mounted = await mountAt(`/apps/${APP_ID}/webhooks`);
        const router = mounted.vm.$router;
        await submitEndpoint(NEW_ENDPOINT);

        vf.showConfirm.mockResolvedValueOnce(false);
        await router.push({ name: 'app-domains', params: { appId: APP_ID } });
        expect(router.currentRoute.value.name).toBe('app-webhooks');
        expect(wrapper!.find<HTMLInputElement>('input[readonly]').element.value).toBe(SECRET);

        vf.showConfirm.mockResolvedValueOnce(true);
        await router.push({ name: 'app-domains', params: { appId: APP_ID } });
        await flushPromises();
        expect(router.currentRoute.value.name).toBe('app-domains');
        expect(document.body.innerHTML).not.toContain(SECRET);
    });

    it('drops a secret that arrives after the page was left and says so in a toast without the secret', async () => {
        lists([]);
        let resolveCreate: (value: unknown) => void = () => undefined;
        api.createAppWebhook.mockReturnValue(new Promise(resolve => (resolveCreate = resolve)));
        const mounted = await mountAt(`/apps/${APP_ID}/webhooks`);
        const router = mounted.vm.$router;
        await submitEndpoint(NEW_ENDPOINT);

        vf.showConfirm.mockResolvedValueOnce(true);
        await router.push('/apps');
        await flushPromises();
        expect(router.currentRoute.value.path).toBe('/apps');

        resolveCreate({ data: { webhook: newHook, secret: SECRET } });
        await flushPromises();
        expect(document.body.innerHTML).not.toContain(SECRET);
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: expect.stringContaining('Replace its secret') }));
        expect(JSON.stringify(vf.showToast.mock.calls)).not.toContain(SECRET);
    });

    it('explains a 409 as the ten-endpoint limit and keeps the form open', async () => {
        lists([]);
        api.createAppWebhook.mockRejectedValue(httpError(409));
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await submitEndpoint(NEW_ENDPOINT);
        expect(wrapper!.text()).toContain('10 webhooks are already enabled. Disable one first.');
        expect(wrapper!.find('form').exists()).toBe(true);
    });

    it.each([
        ['a 503', () => httpError(503)],
        ['no response', () => networkError()],
        ['a 500', () => httpError(500)]
    ])('reports an unknown outcome and shows no secret after %s', async (_label, error) => {
        lists([]);
        api.createAppWebhook.mockRejectedValue(error());
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await submitEndpoint(NEW_ENDPOINT);
        expect(wrapper!.text()).toContain("The list was reloaded. If the webhook is listed, replace its secret to get one, and don't add it again.");
        expect(wrapper!.find('form').exists()).toBe(false);
        expect(api.listAppWebhooks).toHaveBeenCalledTimes(2);
        expect(buttonIn(wrapper!, 'Add webhook').attributes('disabled')).toBeUndefined();
        expect(wrapper!.find('input[readonly]').exists()).toBe(false);
    });

    it('treats a success without a usable secret as an error', async () => {
        lists([]);
        api.createAppWebhook.mockResolvedValue({ data: { webhook: newHook } });
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await submitEndpoint(NEW_ENDPOINT);
        expect(wrapper!.text()).toContain("didn't return its secret");
        expect(wrapper!.find('input[readonly]').exists()).toBe(false);
    });
});

describe('editing an endpoint', () => {
    it('prefills the form and sends only the changed fields', async () => {
        lists();
        api.updateAppWebhook.mockResolvedValue({ data: { ...enabledHook, description: 'Renamed' } });
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_ENABLED, 'Edit');

        expect(wrapper!.find<HTMLInputElement>('input[inputmode="url"]').element.value).toBe('https://hooks.example.com/enabled');
        expect(wrapper!.find<HTMLInputElement>('input[id$="-description"]').element.value).toBe('Billing receiver');
        expect(wrapper!.find<HTMLInputElement>('input[type="radio"][value="all"]').element.checked).toBe(true);
        expect(wrapper!.text()).toContain('Events already queued will use the new URL on their next attempt.');

        await wrapper!.find('input[id$="-description"]').setValue('Renamed');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.updateAppWebhook).toHaveBeenCalledWith({ path: { appId: APP_ID, webhookId: ID_ENABLED }, body: { description: 'Renamed' } });
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Webhook updated' }));
        expect(wrapper!.find('form').exists()).toBe(false);
        expect(api.listAppWebhooks).toHaveBeenCalledTimes(2);
    });

    it('prefills the selected events, and sends an empty list to go back to all events', async () => {
        lists();
        api.updateAppWebhook.mockResolvedValue({ data: { ...disabledHook, events: undefined } });
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_DISABLED, 'Edit');

        expect(wrapper!.find<HTMLInputElement>('input[type="radio"][value="selected"]').element.checked).toBe(true);
        expect(wrapper!.find<HTMLInputElement>('input[type="checkbox"][value="accepted"]').element.checked).toBe(true);
        expect(wrapper!.find<HTMLInputElement>('input[type="checkbox"][value="clicked"]').element.checked).toBe(false);

        await wrapper!.find('input[type="radio"][value="all"]').setValue(true);
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.updateAppWebhook).toHaveBeenCalledWith({ path: { appId: APP_ID, webhookId: ID_DISABLED }, body: { events: [] } });
    });

    it('sends the endpoint when the URL changed and makes no request when nothing changed', async () => {
        lists();
        api.updateAppWebhook.mockResolvedValue({ data: { ...enabledHook, endpoint: NEW_ENDPOINT } });
        await mountAt(`/apps/${APP_ID}/webhooks`);

        await menuAction(ID_ENABLED, 'Edit');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.updateAppWebhook).not.toHaveBeenCalled();
        expect(wrapper!.find('form').exists()).toBe(false);

        await menuAction(ID_ENABLED, 'Edit');
        await wrapper!.find('input[inputmode="url"]').setValue(NEW_ENDPOINT);
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.updateAppWebhook).toHaveBeenCalledWith({ path: { appId: APP_ID, webhookId: ID_ENABLED }, body: { endpoint: NEW_ENDPOINT } });
    });

    it('keeps the form open and explains a rejected change', async () => {
        lists();
        api.updateAppWebhook.mockRejectedValue(httpError(400));
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_ENABLED, 'Edit');
        await wrapper!.find('input[id$="-description"]').setValue('x');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(wrapper!.text()).toContain('rejected the changes');
        expect(wrapper!.find('form').exists()).toBe(true);
    });
});

describe('rotating, deleting and testing', () => {
    it('rotates after confirmation, shows the new secret once and clears it', async () => {
        lists();
        api.rotateAppWebhookSecret.mockResolvedValue({ data: { webhook: enabledHook, secret: SECRET } });
        vf.showConfirm.mockResolvedValueOnce(false);
        await mountAt(`/apps/${APP_ID}/webhooks`);

        await menuAction(ID_ENABLED, 'Replace secret');
        expect(vf.showConfirm).toHaveBeenCalledWith(
            'Replace signing secret',
            expect.stringMatching(/current secret stops working immediately.*retried for about 20 hours/)
        );
        expect(api.rotateAppWebhookSecret).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(true);
        await menuAction(ID_ENABLED, 'Replace secret');
        expect(api.rotateAppWebhookSecret).toHaveBeenCalledWith({ path: { appId: APP_ID, webhookId: ID_ENABLED } });
        expect(wrapper!.find<HTMLInputElement>('input[readonly]').element.value).toBe(SECRET);
        expect(wrapper!.text()).toContain('Signing secret replaced');

        vf.showConfirm.mockResolvedValueOnce(true);
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(document.body.innerHTML).not.toContain(SECRET);
    });

    it('tells the user to rotate again when a rotation has an uncertain outcome', async () => {
        lists();
        api.rotateAppWebhookSecret.mockRejectedValue(networkError());
        vf.showConfirm.mockResolvedValue(true);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_ENABLED, 'Replace secret');
        expect(wrapper!.text()).toContain('replace it again');
        expect(wrapper!.find('input[readonly]').exists()).toBe(false);
    });

    it('deletes after a destructive confirmation and reloads on 204', async () => {
        lists();
        api.deleteAppWebhook.mockResolvedValue({ data: undefined, response: new Response(null, { status: 204 }) });
        vf.showConfirmDestroy.mockResolvedValueOnce(false);
        await mountAt(`/apps/${APP_ID}/webhooks`);

        await menuAction(ID_DISABLED, 'Delete');
        expect(vf.showConfirmDestroy).toHaveBeenCalledWith(
            'Delete webhook',
            expect.stringMatching(/Queued events are cancelled.*already on its way may still arrive.*history is kept.*can't be undone/i)
        );
        expect(api.deleteAppWebhook).not.toHaveBeenCalled();

        vf.showConfirmDestroy.mockResolvedValueOnce(true);
        lists([enabledHook]);
        await menuAction(ID_DISABLED, 'Delete');
        expect(api.deleteAppWebhook).toHaveBeenCalledWith({ path: { appId: APP_ID, webhookId: ID_DISABLED } });
        expect(api.listAppWebhooks).toHaveBeenCalledTimes(2);
        expect(wrapper!.text()).not.toContain(ID_DISABLED);
    });

    it('says the endpoint was already deleted on 404 and reloads', async () => {
        lists();
        api.deleteAppWebhook.mockRejectedValue(httpError(404));
        vf.showConfirmDestroy.mockResolvedValue(true);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_DISABLED, 'Delete');
        expect(wrapper!.text()).toContain('already deleted');
        expect(api.listAppWebhooks).toHaveBeenCalledTimes(2);
    });

    it.each([
        ['success', { success: true, http_status: 204 }, 'It answered with status 204, so it was accepted.', 'success'],
        ['an HTTP rejection', { success: false, http_status: 401 }, 'It answered with status 401, so it was not accepted.', 'warning'],
        [
            'a blocked destination',
            { success: false, http_status: 0, failure: 'blocked_destination' },
            "Test couldn't reach https://hooks.example.com/enabled: not a public internet address.",
            'warning'
        ]
    ])('renders a test-send result for %s as a status notice', async (_label, result, text, tone) => {
        lists();
        api.testAppWebhook.mockResolvedValue({ data: result });
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await buttonIn(rowFor(ID_ENABLED), 'Send test').trigger('click');
        await flushPromises();
        expect(api.testAppWebhook).toHaveBeenCalledWith({ path: { appId: APP_ID, webhookId: ID_ENABLED } });
        const notice = wrapper!.find('.notice[role="status"]');
        expect(notice.text()).toContain(text);
        expect(notice.classes()).toContain(tone);
        // A test is never a confirmation or a reload of the list.
        expect(vf.showConfirm).not.toHaveBeenCalled();
    });
});

function fireBeforeUnload() {
    const event = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
}

describe('one-time secret leave guard', () => {
    it('guards sign-out and unload while a rotated secret is shown, and releases on unmount', async () => {
        lists();
        api.rotateAppWebhookSecret.mockResolvedValue({ data: { webhook: enabledHook, secret: SECRET } });
        vf.showConfirm.mockResolvedValueOnce(true);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_ENABLED, 'Replace secret');
        expect(wrapper!.find<HTMLInputElement>('input[readonly]').element.value).toBe(SECRET);
        expect(fireBeforeUnload()).toBe(true);

        vf.showConfirm.mockResolvedValueOnce(false);
        await signOut();
        expect(vf.showConfirm).toHaveBeenLastCalledWith('Leave without saving the secret?', expect.any(String));
        expect(authState.status).toBe('signed-in');

        wrapper!.unmount();
        wrapper = undefined;
        expect(fireBeforeUnload()).toBe(false);
        const calls = vf.showConfirm.mock.calls.length;
        await signOut();
        expect(vf.showConfirm).toHaveBeenCalledTimes(calls);
        expect(authState.status).toBe('signed-out');
    });

    it('guards sign-out and unload while a rotate is in flight', async () => {
        lists();
        api.rotateAppWebhookSecret.mockReturnValue(new Promise(() => undefined));
        vf.showConfirm.mockResolvedValueOnce(true);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_ENABLED, 'Replace secret');
        expect(fireBeforeUnload()).toBe(true);
        vf.showConfirm.mockResolvedValueOnce(false);
        await signOut();
        expect(vf.showConfirm).toHaveBeenLastCalledWith('Leave while a signing secret is being created?', expect.any(String));
        expect(authState.status).toBe('signed-in');
    });

    it('shows a toast and never the secret when a rotate response arrives after leaving', async () => {
        lists();
        let resolve!: (value: unknown) => void;
        api.rotateAppWebhookSecret.mockReturnValue(new Promise(r => (resolve = r)));
        vf.showConfirm.mockResolvedValue(true);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_ENABLED, 'Replace secret');
        await router!.push('/apps');
        await flushPromises();
        resolve({ data: { webhook: enabledHook, secret: SECRET } });
        await flushPromises();
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: expect.stringContaining("wasn't shown") }));
        expect(document.body.innerHTML).not.toContain(SECRET);
        expect(fireBeforeUnload()).toBe(false);
    });

    it('does not treat an in-flight edit as a signing secret being issued', async () => {
        lists();
        api.updateAppWebhook.mockReturnValue(new Promise(() => undefined));
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_ENABLED, 'Edit');
        await wrapper!.find('input[id$="-description"]').setValue('changed');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.updateAppWebhook).toHaveBeenCalled();
        expect(fireBeforeUnload()).toBe(false);
        await signOut();
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(authState.status).toBe('signed-out');
    });
});

describe('edit errors', () => {
    it('uses edit-specific copy for a 404 and has no ten-endpoint text', async () => {
        lists();
        api.updateAppWebhook.mockRejectedValue(httpError(404));
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_ENABLED, 'Edit');
        await wrapper!.find('input[id$="-description"]').setValue('changed');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(wrapper!.text()).toContain('It may have been deleted.');
        expect(wrapper!.text()).not.toContain('inactive');
    });
});

describe('add availability', () => {
    it('disables Add with a hint when the list is unavailable (503)', async () => {
        api.listAppWebhooks.mockRejectedValue(httpError(503));
        await mountAt(`/apps/${APP_ID}/webhooks`);
        const add = buttonIn(wrapper!, 'Add webhook');
        expect(add.attributes('disabled')).toBeDefined();
        expect(add.attributes('aria-describedby')).toBe('add-hint');
        expect(wrapper!.find('#add-hint').text()).toContain("The webhook list hasn't loaded");
    });
});

describe('overflow menu disabled items', () => {
    it('marks items aria-disabled and ignores clicks while a mutation is running', async () => {
        lists();
        api.testAppWebhook.mockReturnValue(new Promise(() => undefined));
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await buttonIn(rowFor(ID_ENABLED), 'Send test').trigger('click');
        await flushPromises();
        await rowFor(ID_DISABLED).find('.dropdown-trigger').trigger('click');
        await flushPromises();
        for (const label of ['Edit', 'Replace secret', 'Delete']) {
            const item = buttonIn(rowFor(ID_DISABLED), label);
            expect(item.attributes('aria-disabled')).toBe('true');
            expect(item.attributes('disabled')).toBeUndefined();
        }
        await buttonIn(rowFor(ID_DISABLED), 'Edit').trigger('click');
        await flushPromises();
        expect(wrapper!.find('form').exists()).toBe(false);
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(vf.showConfirmDestroy).not.toHaveBeenCalled();
    });
});

const ID_POSTMARK = '44444444444444444444444444444444';
const postmarkHook = {
    id: ID_POSTMARK,
    format: 'postmark',
    endpoint: 'https://hooks.example.com/postmark',
    description: '',
    events: ['Delivery', 'Bounce'],
    active: true,
    created_at: '2026-10-02T12:00:00Z',
    auth: { username: 'thinpost', header_names: ['X-Api-Key'] }
};
const ID_BARE = '55555555555555555555555555555555';
const bareHook = { ...postmarkHook, id: ID_BARE, endpoint: 'https://hooks.example.com/bare', events: undefined, auth: undefined };

async function openAuth() {
    const toggle = buttonIn(wrapper!, 'HTTP authentication');
    if (toggle.attributes('aria-expanded') !== 'true') await toggle.trigger('click');
}

async function openCreateModal() {
    await buttonIn(wrapper!, 'Add webhook').trigger('click');
    await flushPromises();
}

describe('provider formats', () => {
    it('shows each endpoint’s format, event summary, secret state and authentication', async () => {
        lists([enabledHook, postmarkHook, bareHook]);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        const row = rowFor(ID_POSTMARK);
        expect(row.find('.tag').text()).toBe('Postmark');
        expect(row.text()).toContain('2 event types');
        expect(row.text()).toContain('Unsigned');
        expect(row.text()).not.toContain('Created');
        expect(row.text()).toContain('Basic · 1 header');
        expect(row.text()).not.toContain('Add Basic auth');
        expect(rowFor(ID_BARE).text()).toContain('Unsigned. Add Basic auth or a header so your receiver can trust it.');
        expect(rowFor(ID_ENABLED).text()).toContain('Thinpost');
        expect(rowFor(ID_ENABLED).text()).toContain('Created');
    });

    it('offers each format’s own events and resets the filter when the format changes', async () => {
        lists([]);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await openCreateModal();
        const select = wrapper!.find('select');
        expect(select.findAll('option').map(o => o.text())).toEqual(['Thinpost', 'Postmark', 'Resend', 'Mailgun', 'Inbound mail (Thinpost)']);

        await wrapper!.find('input[type="radio"][value="selected"]').setValue(true);
        await wrapper!.find('input[type="checkbox"][value="accepted"]').setValue(true);
        await select.setValue('postmark');
        expect(wrapper!.find<HTMLInputElement>('input[type="radio"][value="all"]').element.checked).toBe(true);
        await wrapper!.find('input[type="radio"][value="selected"]').setValue(true);
        expect(wrapper!.findAll<HTMLInputElement>('input[type="checkbox"]').map(i => i.element.value)).toEqual(['Delivery', 'Bounce', 'Open', 'SubscriptionChange', 'SpamComplaint']);
        expect(wrapper!.find('input[type="checkbox"][value="accepted"]').exists()).toBe(false);

        await select.setValue('resend');
        await wrapper!.find('input[type="radio"][value="selected"]').setValue(true);
        expect(wrapper!.find('input[type="checkbox"][value="email.delivery_delayed"]').exists()).toBe(true);
        await select.setValue('mailgun');
        await wrapper!.find('input[type="radio"][value="selected"]').setValue(true);
        expect(wrapper!.find('input[type="checkbox"][value="permanent_fail"]').exists()).toBe(true);
    });

    it('explains the inbound mail format and hides its event filter', async () => {
        lists([]);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await openCreateModal();
        expect(wrapper!.find('[data-testid="inbound-format-help"]').exists()).toBe(false);
        expect(wrapper!.find('fieldset.events').exists()).toBe(true);
        await wrapper!.find('select').setValue('thinpost_inbound');
        const help = wrapper!.get('[data-testid="inbound-format-help"]').text();
        expect(help).toContain('several megabytes');
        expect(help).toContain('2 minutes');
        expect(help).toContain('signed');
        expect(help).toContain('more than once');
        expect(wrapper!.find('fieldset.events').exists()).toBe(false);
        expect(wrapper!.find('input[type="radio"]').exists()).toBe(false);
        expect(wrapper!.text()).not.toContain('Unsigned.');
        await wrapper!.find('select').setValue('postmark');
        expect(wrapper!.find('fieldset.events').exists()).toBe(true);
    });

    it('creates an inbound mail endpoint with no event filter and shows its signing secret', async () => {
        lists([]);
        const inboundHook = { ...hook('66666666666666666666666666666666', 'https://hooks.example.com/in', true), format: 'thinpost_inbound' };
        api.createAppWebhook.mockResolvedValue({ data: { webhook: inboundHook, secret: 'tpwh_secret_value' } });
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await openCreateModal();
        await wrapper!.find('select').setValue('thinpost_inbound');
        await wrapper!.find('input[inputmode="url"]').setValue('https://hooks.example.com/in');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.createAppWebhook).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { format: 'thinpost_inbound', endpoint: 'https://hooks.example.com/in', description: '' } });
        expect(wrapper!.text()).toContain('Signing secret created');
        expect(wrapper!.find<HTMLInputElement>('input[readonly]').element.value).toBe('tpwh_secret_value');
    });

    it('shows an inbound mail endpoint as receiving mail, not as all events', async () => {
        const inboundHook = { ...hook('66666666666666666666666666666666', 'https://hooks.example.com/in', true), format: 'thinpost_inbound' };
        lists([inboundHook]);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        const row = rowFor('66666666666666666666666666666666');
        expect(row.find('.tag').text()).toBe('Inbound mail');
        expect(row.text()).toContain('Received mail');
        expect(row.text()).not.toContain('All events');
    });

    it('shows the format read-only when editing', async () => {
        lists([postmarkHook]);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_POSTMARK, 'Edit');
        expect(wrapper!.find('select').exists()).toBe(false);
        expect(wrapper!.find('form .format-fixed').text()).toBe('Postmark');
        expect(wrapper!.find<HTMLInputElement>('input[type="checkbox"][value="Delivery"]').element.checked).toBe(true);
    });

    it('creates a Postmark endpoint with Basic auth and a header, and shows no secret', async () => {
        lists([]);
        api.createAppWebhook.mockResolvedValue({ data: { webhook: postmarkHook } });
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await openCreateModal();
        await wrapper!.find('select').setValue('postmark');
        expect(wrapper!.text()).toContain('Unsigned. Add Basic auth or a header so your receiver can trust it.');
        await wrapper!.find('input[inputmode="url"]').setValue(NEW_ENDPOINT);
        await openAuth();
        await wrapper!.find('input[id$="-user"]').setValue('thinpost');
        await wrapper!.find('input[id$="-pass"]').setValue('p4ss:w0rd');
        expect(wrapper!.text()).not.toContain('Unsigned. Add Basic auth');
        await buttonIn(wrapper!, 'Add header').trigger('click');
        await wrapper!.find('input[id*="-hn-"]').setValue('X-Api-Key');
        await wrapper!.find('input[id*="-hv-"]').setValue('k3y');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();

        expect(api.createAppWebhook).toHaveBeenCalledWith({
            path: { appId: APP_ID },
            body: {
                format: 'postmark',
                endpoint: NEW_ENDPOINT,
                description: '',
                auth: { username: 'thinpost', password: 'p4ss:w0rd', headers: [{ name: 'X-Api-Key', value: 'k3y' }] }
            }
        });
        expect(wrapper!.find('input[readonly]').exists()).toBe(false);
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Webhook added' }));
        expect(document.body.innerHTML).not.toContain('p4ss:w0rd');
        expect(document.body.innerHTML).not.toContain('k3y');
    });

    it('moves URL user information into the Basic fields and never sends it', async () => {
        lists([]);
        api.createAppWebhook.mockResolvedValue({ data: { webhook: postmarkHook } });
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await openCreateModal();
        await wrapper!.find('select').setValue('postmark');
        await wrapper!.find('input[inputmode="url"]').setValue('https://us%40er:p%3Ass@hooks.example.com/pm?x=1');
        await flushPromises();
        expect(wrapper!.find<HTMLInputElement>('input[inputmode="url"]').element.value).toBe('https://hooks.example.com/pm?x=1');
        expect(wrapper!.find<HTMLInputElement>('input[id$="-user"]').element.value).toBe('us@er');
        expect(wrapper!.find<HTMLInputElement>('input[id$="-pass"]').element.value).toBe('p:ss');
        expect(wrapper!.text()).toContain('The user name and password were moved to Basic authentication.');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        const body = api.createAppWebhook.mock.calls[0]![0].body;
        expect(body.endpoint).toBe('https://hooks.example.com/pm?x=1');
        expect(body.auth).toEqual({ username: 'us@er', password: 'p:ss' });
    });

    it('rejects reserved or underscored header names and a password-less user without calling the API', async () => {
        lists([]);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await openCreateModal();
        await wrapper!.find('input[inputmode="url"]').setValue(NEW_ENDPOINT);
        await openAuth();
        await wrapper!.find('input[id$="-user"]').setValue('u');
        await buttonIn(wrapper!, 'Add header').trigger('click');
        await wrapper!.find('input[id*="-hn-"]').setValue('X_Bad');
        await wrapper!.find('input[id*="-hv-"]').setValue('v');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.createAppWebhook).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('Enter a password.');
        expect(wrapper!.text()).toContain('Header names cannot contain underscores.');
        await wrapper!.find('input[id*="-hn-"]').setValue('X-Pm-Trace');
        await flushPromises();
        expect(wrapper!.text()).toContain("This header name is reserved and can't be used.");
    });

    it('sends no auth on edit when the credentials were left blank, and replaces them when changed', async () => {
        lists([postmarkHook]);
        api.updateAppWebhook.mockResolvedValue({ data: postmarkHook });
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_POSTMARK, 'Edit');
        await openAuth();
        expect(wrapper!.find<HTMLInputElement>('input[id$="-user"]').element.value).toBe('thinpost');
        expect(wrapper!.find<HTMLInputElement>('input[id*="-hn-"]').element.value).toBe('X-Api-Key');
        expect(wrapper!.find<HTMLInputElement>('input[id$="-pass"]').element.value).toBe('');
        await wrapper!.find('input[id$="-description"]').setValue('Renamed');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.updateAppWebhook).toHaveBeenLastCalledWith({ path: { appId: APP_ID, webhookId: ID_POSTMARK }, body: { description: 'Renamed' } });

        await menuAction(ID_POSTMARK, 'Edit');
        await openAuth();
        await wrapper!.find('input[id$="-pass"]').setValue('new-pass');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        // Every value must be re-entered once anything changes.
        expect(wrapper!.text()).toContain('Enter a header value.');
        await wrapper!.find('input[id*="-hv-"]').setValue('new-key');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.updateAppWebhook).toHaveBeenLastCalledWith({
            path: { appId: APP_ID, webhookId: ID_POSTMARK },
            body: { auth: { username: 'thinpost', password: 'new-pass', headers: [{ name: 'X-Api-Key', value: 'new-key' }] } }
        });
    });

    it('removes authentication explicitly with an empty auth object', async () => {
        lists([postmarkHook]);
        api.updateAppWebhook.mockResolvedValue({ data: { ...postmarkHook, auth: undefined } });
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await menuAction(ID_POSTMARK, 'Edit');
        await openAuth();
        await buttonIn(wrapper!, 'Remove authentication').trigger('click');
        expect(wrapper!.text()).toContain('Unsigned. Add Basic auth or a header so your receiver can trust it.');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.updateAppWebhook).toHaveBeenCalledWith({ path: { appId: APP_ID, webhookId: ID_POSTMARK }, body: { auth: {} } });
    });

    it('shows the Resend secret with its own usage text', async () => {
        lists([]);
        const resendHook = { ...enabledHook, format: 'resend' };
        api.createAppWebhook.mockResolvedValue({ data: { webhook: resendHook, secret: 'whsec_c2VjcmV0' } });
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await openCreateModal();
        await wrapper!.find('select').setValue('resend');
        await wrapper!.find('input[inputmode="url"]').setValue(NEW_ENDPOINT);
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.createAppWebhook.mock.calls[0]![0].body.format).toBe('resend');
        expect(wrapper!.find<HTMLInputElement>('input[readonly]').element.value).toBe('whsec_c2VjcmV0');
        expect(wrapper!.text()).toContain('resend.webhooks.verify');
    });

    it('hides Replace secret for Postmark and explains a 409 from replacing', async () => {
        lists([postmarkHook, enabledHook]);
        api.rotateAppWebhookSecret.mockRejectedValue(httpError(409));
        vf.showConfirm.mockResolvedValue(true);
        await mountAt(`/apps/${APP_ID}/webhooks`);
        await rowFor(ID_POSTMARK).find('.dropdown-trigger').trigger('click');
        await flushPromises();
        expect(rowFor(ID_POSTMARK).findAll('button').some(b => b.text().includes('Replace secret'))).toBe(false);

        await menuAction(ID_ENABLED, 'Replace secret');
        expect(wrapper!.text()).toContain('no signing secret to replace');
    });
});

describe('add dialog opened from the Inbound tab', () => {
    it('opens with the inbound format chosen, keeps the format editable and drops the query', async () => {
        lists([]);
        await mountAt(`/apps/${APP_ID}/webhooks?add=thinpost_inbound`);
        await flushPromises();
        const select = wrapper!.find<HTMLSelectElement>('select');
        expect(select.exists()).toBe(true);
        expect(select.element.value).toBe('thinpost_inbound');
        expect(wrapper!.find('[data-testid="inbound-format-help"]').exists()).toBe(true);
        // Editable: another format can still be picked.
        await select.setValue('resend');
        expect(wrapper!.find('[data-testid="inbound-format-help"]').exists()).toBe(false);
        expect(router!.currentRoute.value.query).toEqual({});
    });

    it('creates an inbound endpoint from the preset like any other', async () => {
        lists([]);
        const inboundHook = { ...enabledHook, format: 'thinpost_inbound' };
        api.createAppWebhook.mockResolvedValue({ data: { webhook: inboundHook, secret: 'tpwh_secret_value' } });
        await mountAt(`/apps/${APP_ID}/webhooks?add=thinpost_inbound`);
        await wrapper!.find('input[inputmode="url"]').setValue(NEW_ENDPOINT);
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.createAppWebhook.mock.calls[0]![0].body.format).toBe('thinpost_inbound');
        expect(wrapper!.find<HTMLInputElement>('input[readonly]').element.value).toBe('tpwh_secret_value');
    });

    it('does not open a dialog without the request, and not for an unknown value', async () => {
        lists([]);
        await mountAt(`/apps/${APP_ID}/webhooks?add=postmark`);
        await flushPromises();
        expect(wrapper!.find('select').exists()).toBe(false);
    });

    it('does not open the dialog for a paused app', async () => {
        lists([]);
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        await mountAt(`/apps/${APP_ID}/webhooks?add=thinpost_inbound`);
        await flushPromises();
        expect(wrapper!.find('select').exists()).toBe(false);
    });
});
