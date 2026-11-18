import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import { installVf } from '@zyno-io/vue-foundation';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createMemoryHistory, createRouter } from 'vue-router';

const api = vi.hoisted(() => ({ listApps: vi.fn() }));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const auth = vi.hoisted(() => ({ signOut: vi.fn(), signIn: vi.fn() }));
vi.mock('@/auth', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...auth };
});

const { authState } = await import('@/auth');
const { forgetApp, rememberApp } = await import('@/last-app');
const { appRoute } = await import('@/routes');
const { openModalCount } = await import('@/shared/modal-state');
const { default: Layout } = await import('@/shared/components/layout.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const OTHER_ID = 'fedcba9876543210fedcba9876543210';
const apps = [
    { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' },
    { id: OTHER_ID, name: 'Marketing', active: true, created_at: '2026-10-02T12:00:00Z' }
];

let wrapper: VueWrapper | undefined;

const stub = { template: '<div />' };

async function mountAt(path: string) {
    const router = createRouter({
        history: createMemoryHistory(),
        routes: [
            { path: '/apps', name: 'apps', component: stub, meta: { nav: 'apps' } },
            appRoute({ 'app-keys': stub, 'app-domains': stub, 'app-messages': stub, 'app-message': stub, 'app-webhooks': stub, 'app-tracking': stub, 'app-suppressions': stub, 'app-inbound': stub, 'app-inbound-message': stub }),
            { path: '/domains', name: 'domains', component: stub, meta: { nav: 'domains' } },
            { path: '/messages', name: 'messages', component: stub, meta: { nav: 'messages' } },
            { path: '/webhooks', name: 'webhooks', component: stub, meta: { nav: 'webhooks' } },
            { path: '/tracking', name: 'tracking', component: stub, meta: { nav: 'tracking' } },
            { path: '/operations', name: 'operations', component: stub, meta: { nav: 'operations' } }
        ]
    });
    await router.push(path);
    wrapper = mount(
        { components: { Layout }, template: '<Layout><div id="sentinel">content</div><RouterView /></Layout>' },
        { global: { plugins: [router, installVf] }, attachTo: document.body }
    );
    await flushPromises();
    return router;
}

function navHref(label: string) {
    const link = wrapper!.findAll('nav[aria-label="Primary"] a').find(a => a.text().startsWith(label));
    if (!link) throw new Error(`nav link "${label}" not found`);
    return link;
}

beforeEach(() => {
    api.listApps.mockReset();
    api.listApps.mockResolvedValue({ data: { apps } });
    auth.signOut.mockReset();
    auth.signIn.mockReset();
    authState.status = 'signed-in';
    authState.displayName = 'Ada Operator';
    authState.issuer = 'https://login.example.com/tenant';
    authState.notice = null;
    openModalCount.value = 0;
    localStorage.clear();
    forgetApp(APP_ID);
    forgetApp(OTHER_ID);
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
    openModalCount.value = 0;
    authState.displayName = null;
    authState.issuer = null;
});

describe('layout navigation', () => {
    it('links app-scoped entries to the remembered app', async () => {
        rememberApp(APP_ID);
        await mountAt('/apps');
        expect(navHref('Sending domains').attributes('href')).toBe(`/apps/${APP_ID}/domains`);
        expect(navHref('Messages').attributes('href')).toBe(`/apps/${APP_ID}/messages`);
        expect(navHref('Webhooks').attributes('href')).toBe(`/apps/${APP_ID}/webhooks`);
        expect(navHref('Apps').attributes('href')).toBe('/apps');
    });

    it('falls back to the picker paths when no app is remembered', async () => {
        await mountAt('/apps');
        expect(navHref('Sending domains').attributes('href')).toBe('/domains');
        expect(navHref('Messages').attributes('href')).toBe('/messages');
        expect(navHref('Webhooks').attributes('href')).toBe('/webhooks');
    });

    it('prefers the app on screen over the remembered one', async () => {
        await mountAt(`/apps/${APP_ID}`);
        rememberApp(OTHER_ID);
        await flushPromises();
        expect(navHref('Messages').attributes('href')).toBe(`/apps/${APP_ID}/messages`);
    });

    it('marks the active item from the route meta', async () => {
        const router = await mountAt(`/apps/${APP_ID}/messages`);
        expect(navHref('Messages').classes()).toContain('active');
        expect(navHref('Messages').attributes('aria-current')).toBe('page');
        expect(navHref('Webhooks').classes()).not.toContain('active');

        await router.push('/apps');
        await flushPromises();
        expect(navHref('Apps').classes()).toContain('active');
        expect(navHref('Messages').classes()).not.toContain('active');
    });

    it('links Tracking to the app on screen and Operations globally, with no unavailable entries', async () => {
        const router = await mountAt(`/apps/${APP_ID}/tracking`);
        expect(navHref('Tracking').attributes('href')).toBe(`/apps/${APP_ID}/tracking`);
        expect(navHref('Tracking').classes()).toContain('active');
        expect(navHref('Operations').attributes('href')).toBe('/operations');
        expect(wrapper!.find('nav[aria-label="Primary"] .unavailable').exists()).toBe(false);
        expect(wrapper!.text()).not.toContain('not yet available');
        await router.push('/operations');
        await flushPromises();
        expect(navHref('Operations').classes()).toContain('active');
    });
});

describe('layout shell', () => {
    it('makes the topbar and the session banner inert while a modal is open', async () => {
        authState.status = 'ended';
        authState.notice = 'Your session ended.';
        await mountAt('/apps');
        expect(wrapper!.find('.topbar').attributes('inert')).toBeUndefined();
        expect(wrapper!.find('.session-ended').attributes('inert')).toBeUndefined();

        openModalCount.value = 1;
        await flushPromises();
        expect(wrapper!.find('.topbar').attributes('inert')).toBeDefined();
        expect(wrapper!.find('.session-ended').attributes('inert')).toBeDefined();
    });

    it('shows the sign-in banner when the session ended and keeps the content mounted', async () => {
        authState.status = 'ended';
        authState.notice = 'Your session ended.';
        await mountAt('/apps');
        const banner = wrapper!.find('.session-ended');
        expect(banner.attributes('role')).toBe('alert');
        expect(banner.text()).toContain('Your session ended.');
        await banner.find('button').trigger('click');
        expect(auth.signIn).toHaveBeenCalledTimes(1);
        expect(wrapper!.find('#sentinel').exists()).toBe(true);
    });

    it('renders the theme and account menus and signs out from the account menu', async () => {
        await mountAt('/apps');
        expect(wrapper!.find('button[aria-label="Theme"]').exists()).toBe(true);
        const account = wrapper!.find('button[aria-label="Account"]');
        expect(account.exists()).toBe(true);

        await account.trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('Ada Operator');
        expect(wrapper!.text()).toContain('login.example.com');
        const signOutItem = wrapper!.findAll('[role="menuitem"]').find(i => i.text().includes('Sign out'));
        expect(signOutItem).toBeDefined();
        await signOutItem!.trigger('click');
        expect(auth.signOut).toHaveBeenCalledTimes(1);
    });

    it('offers Sign in again from the account menu when the session ended', async () => {
        authState.status = 'ended';
        authState.notice = 'Your session ended.';
        await mountAt('/apps');
        await wrapper!.find('button[aria-label="Account"]').trigger('click');
        await flushPromises();
        const item = wrapper!.findAll('[role="menuitem"]').find(i => i.text().includes('Sign in again'));
        expect(item).toBeDefined();
        expect(wrapper!.findAll('[role="menuitem"]').some(i => i.text().includes('Sign out'))).toBe(false);
    });
});
