import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import { installVf } from '@zyno-io/vue-foundation';
import type { Component } from 'vue';
import { createMemoryHistory, createRouter, type Router } from 'vue-router';

import { appRoute, type AppScreenName } from '@/routes';

const SCREEN_NAMES: AppScreenName[] = ['app-keys', 'app-domains', 'app-messages', 'app-message', 'app-webhooks', 'app-tracking', 'app-suppressions', 'app-inbound', 'app-inbound-message', 'app-smtp'];

/**
 * Mounts a route inside the real nested application shell (`appRoute`), with stubs for every
 * section that the test does not supply.
 */
export async function mountAppRoute(
    path: string,
    screens: Partial<Record<AppScreenName, Component>>
): Promise<{ wrapper: VueWrapper; router: Router }> {
    const stub = (name: string): Component => ({ template: `<div data-stub="${name}" />` });
    const resolved = Object.fromEntries(SCREEN_NAMES.map(name => [name, screens[name] ?? stub(name)])) as Record<AppScreenName, Component>;
    const router = createRouter({
        history: createMemoryHistory(),
        routes: [{ path: '/apps', name: 'apps', component: { template: '<div>apps</div>' } }, appRoute(resolved)]
    });
    await router.push(path);
    const wrapper = mount({ template: '<RouterView />' }, { global: { plugins: [router, installVf] }, attachTo: document.body });
    await flushPromises();
    return { wrapper, router };
}
