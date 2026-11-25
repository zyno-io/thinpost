import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { defineComponent, h, onMounted, onUnmounted } from 'vue';

import { httpError } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({ listApps: vi.fn() }));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const { authState } = await import('@/auth');
const { useAppContext } = await import('@/app-context');
const { forgetApp, rememberedAppId } = await import('@/last-app');
const { openModalCount } = await import('@/shared/modal-state');

const APP_ID = '0123456789abcdef0123456789abcdef';
const OTHER_ID = 'fedcba9876543210fedcba9876543210';
const MESSAGE_ID = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
const billing = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const other = { id: OTHER_ID, name: 'Marketing', active: false, created_at: '2026-10-02T12:00:00Z' };

const mounts = vi.fn();
const unmounts = vi.fn();

function stub(name: string) {
    return defineComponent({
        name: `Stub${name}`,
        setup() {
            const context = useAppContext();
            onMounted(() => mounts(name, context.appId.value));
            onUnmounted(() => unmounts(name));
            return () => h('div', { 'data-stub': name }, `${name}:${context.app.value.name}`);
        }
    });
}

const screens = {
    'app-keys': stub('keys'),
    'app-domains': stub('domains'),
    'app-messages': stub('messages'),
    'app-message': stub('message'),
    'app-webhooks': stub('webhooks'),
    'app-suppressions': stub('suppressions'),
    'app-inbound': stub('inbound')
};

let wrapper: VueWrapper | undefined;

async function mountAt(path: string, overrides: Parameters<typeof mountAppRoute>[1] = {}) {
    const mounted = await mountAppRoute(path, { ...screens, ...overrides });
    wrapper = mounted.wrapper;
    return mounted;
}

beforeEach(() => {
    api.listApps.mockReset();
    mounts.mockReset();
    unmounts.mockReset();
    authState.status = 'signed-in';
    openModalCount.value = 0;
    localStorage.clear();
    forgetApp(APP_ID);
    forgetApp(OTHER_ID);
    api.listApps.mockResolvedValue({ data: { apps: [billing, other] } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
    openModalCount.value = 0;
});

describe('app shell', () => {
    it('loads the app once and keeps the header mounted across sections', async () => {
        const { router } = await mountAt(`/apps/${APP_ID}`);
        const header = wrapper!.find('.app-header').element;
        expect(wrapper!.text()).toContain('keys:Billing');

        for (const name of ['app-domains', 'app-messages', 'app-webhooks'] as const) {
            await router.push({ name, params: { appId: APP_ID } });
            await flushPromises();
            expect(wrapper!.find('.app-header').element).toBe(header);
        }
        expect(wrapper!.text()).toContain('webhooks:Billing');
        expect(api.listApps).toHaveBeenCalledTimes(1);
    });

    it('shows a skeleton instead of a placeholder crumb while the app loads', async () => {
        let resolve: (value: unknown) => void = () => undefined;
        api.listApps.mockReturnValue(new Promise(r => (resolve = r)));
        await mountAt(`/apps/${APP_ID}/domains`);

        const crumbs = wrapper!.find('.crumbs');
        expect(crumbs.find('.skeleton').exists()).toBe(true);
        expect(crumbs.text().replace('Apps', '')).not.toContain('App');
        expect(wrapper!.find('[data-stub]').exists()).toBe(false);

        resolve({ data: { apps: [billing] } });
        await flushPromises();
        expect(wrapper!.find('.crumbs').find('.skeleton').exists()).toBe(false);
        expect(wrapper!.find('.crumbs').text()).toContain('Billing');
    });

    it('treats a malformed app ID as not found without calling the API', async () => {
        await mountAt('/apps/not-an-id');
        expect(wrapper!.text()).toContain('App not found');
        expect(api.listApps).not.toHaveBeenCalled();
    });

    it('treats an unknown app as not found and forgets a matching remembered ID', async () => {
        localStorage.setItem('thinpost:last-app', OTHER_ID);
        api.listApps.mockResolvedValue({ data: { apps: [billing] } });
        await mountAt(`/apps/${OTHER_ID}`);
        expect(wrapper!.text()).toContain('App not found');
        expect(wrapper!.find('.app-header').exists()).toBe(false);
        expect(localStorage.getItem('thinpost:last-app')).toBeNull();
        expect(rememberedAppId()).toBeNull();
    });

    it('shows a load error and recovers on retry', async () => {
        api.listApps.mockRejectedValueOnce(httpError(503));
        await mountAt(`/apps/${APP_ID}`);
        expect(wrapper!.find('.api-error').text()).toContain('unavailable');
        expect(wrapper!.find('[data-stub]').exists()).toBe(false);

        await wrapper!.find('.api-error button').trigger('click');
        await flushPromises();
        expect(wrapper!.find('.api-error').exists()).toBe(false);
        expect(wrapper!.text()).toContain('keys:Billing');
        expect(api.listApps).toHaveBeenCalledTimes(2);
    });

    it('remembers the app only after a successful load', async () => {
        api.listApps.mockRejectedValueOnce(httpError(503));
        await mountAt(`/apps/${APP_ID}`);
        expect(localStorage.getItem('thinpost:last-app')).toBeNull();

        await wrapper!.find('.api-error button').trigger('click');
        await flushPromises();
        expect(localStorage.getItem('thinpost:last-app')).toBe(APP_ID);
    });

    it('lists apps in the switcher and targets the same section', async () => {
        const { router } = await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await wrapper!.find('.crumb-switcher').trigger('click');
        await flushPromises();

        const items = wrapper!.findAll('[role="menuitemradio"]');
        expect(items.map(i => i.text())).toEqual(['Billing', 'MarketingPaused']);
        expect(items[0]!.attributes('aria-checked')).toBe('true');
        expect(items[1]!.attributes('aria-checked')).toBe('false');
        expect(items[1]!.attributes('href')).toBe(`/apps/${OTHER_ID}/messages`);

        await items[1]!.trigger('click');
        await flushPromises();
        expect(router.currentRoute.value.fullPath).toBe(`/apps/${OTHER_ID}/messages`);
    });

    it('lists switcher apps by name, ignoring case', async () => {
        const zeta = { ...other, id: 'b'.repeat(32), name: 'zeta', active: true };
        api.listApps.mockResolvedValue({ data: { apps: [zeta, other, billing] } });
        await mountAt(`/apps/${APP_ID}`);
        await wrapper!.find('.crumb-switcher').trigger('click');
        await flushPromises();

        expect(wrapper!.findAll('[role="menuitemradio"]').map(i => i.text())).toEqual(['Billing', 'MarketingPaused', 'zeta']);
    });

    it('remounts a section when the app changes, not when the tab changes', async () => {
        const { router } = await mountAt(`/apps/${APP_ID}/domains`);
        expect(mounts).toHaveBeenCalledWith('domains', APP_ID);

        await router.push({ name: 'app-domains', params: { appId: OTHER_ID } });
        await flushPromises();
        expect(unmounts).toHaveBeenCalledWith('domains');
        expect(mounts).toHaveBeenLastCalledWith('domains', OTHER_ID);
        expect(wrapper!.text()).toContain('domains:Marketing');

        mounts.mockClear();
        unmounts.mockClear();
        await router.push({ name: 'app-webhooks', params: { appId: OTHER_ID } });
        await flushPromises();
        expect(unmounts).toHaveBeenCalledTimes(1);
        expect(unmounts).toHaveBeenCalledWith('domains');
        expect(mounts).toHaveBeenCalledWith('webhooks', OTHER_ID);
        expect(api.listApps).toHaveBeenCalledTimes(2);
    });

    it('makes the header inert while a modal is open', async () => {
        await mountAt(`/apps/${APP_ID}`);
        expect(wrapper!.find('.app-header').attributes('inert')).toBeUndefined();
        openModalCount.value = 1;
        await flushPromises();
        expect(wrapper!.find('.app-header').attributes('inert')).toBeDefined();
    });

    it('keeps the section mounted when the session ends', async () => {
        await mountAt(`/apps/${APP_ID}`);
        authState.status = 'ended';
        await flushPromises();
        expect(wrapper!.find('[data-stub="keys"]').exists()).toBe(true);
        expect(unmounts).not.toHaveBeenCalled();
    });

    it('lets a section reload the app and updates the header', async () => {
        let reloadSection: (() => Promise<unknown>) | undefined;
        const reloader = defineComponent({
            setup() {
                reloadSection = useAppContext().reload;
                return () => h('div', { 'data-stub': 'reloader' });
            }
        });
        await mountAt(`/apps/${APP_ID}`, { 'app-keys': reloader });
        expect(wrapper!.find('.app-status').text()).toBe('Active');

        api.listApps.mockResolvedValue({ data: { apps: [{ ...billing, active: false }, other] } });
        const found = await reloadSection!();
        await flushPromises();
        expect(found).toMatchObject({ id: APP_ID, active: false });
        expect(wrapper!.find('.app-status').text()).toBe('Paused');
        expect(wrapper!.find('[data-stub="reloader"]').exists()).toBe(true);
    });
});
