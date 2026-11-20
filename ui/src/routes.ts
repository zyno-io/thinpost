import type { Component } from 'vue';
import type { RouteRecordRaw } from 'vue-router';

import type { AppSection } from './app-sections';
import type { NavId } from './nav';
import AppShell from './screens/app-shell.vue';

declare module 'vue-router' {
    interface RouteMeta {
        title?: string;
        section?: AppSection;
        nav?: NavId;
    }
}

export type AppScreenName = 'app-keys' | 'app-domains' | 'app-messages' | 'app-message' | 'app-webhooks' | 'app-tracking' | 'app-suppressions' | 'app-inbound' | 'app-inbound-message' | 'app-smtp';

/**
 * The nested `/apps/:appId` record. The shell loads the app once and the
 * section screens render inside it, so it is shared by the router and the tests.
 */
export function appRoute(screens: Record<AppScreenName, Component>): RouteRecordRaw {
    return {
        path: '/apps/:appId',
        component: AppShell,
        children: [
            { path: '', name: 'app-keys', component: screens['app-keys'], meta: { title: 'API keys', section: 'keys', nav: 'apps' } },
            { path: 'domains', name: 'app-domains', component: screens['app-domains'], meta: { title: 'Sending domains', section: 'domains', nav: 'domains' } },
            { path: 'messages', name: 'app-messages', component: screens['app-messages'], meta: { title: 'Messages', section: 'messages', nav: 'messages' } },
            {
                path: 'messages/:messageId',
                name: 'app-message',
                component: screens['app-message'],
                meta: { title: 'Message', section: 'messages', nav: 'messages' }
            },
            { path: 'webhooks', name: 'app-webhooks', component: screens['app-webhooks'], meta: { title: 'Webhooks', section: 'webhooks', nav: 'webhooks' } },
            { path: 'tracking', name: 'app-tracking', component: screens['app-tracking'], meta: { title: 'Tracking', section: 'tracking', nav: 'tracking' } },
            {
                path: 'suppressions',
                name: 'app-suppressions',
                component: screens['app-suppressions'],
                meta: { title: 'Suppressions', section: 'suppressions', nav: 'apps' }
            },
            { path: 'inbound', name: 'app-inbound', component: screens['app-inbound'], meta: { title: 'Inbound', section: 'inbound', nav: 'inbound' } },
            {
                path: 'inbound/:inboundId',
                name: 'app-inbound-message',
                component: screens['app-inbound-message'],
                meta: { title: 'Inbound message', section: 'inbound', nav: 'inbound' }
            },
            { path: 'smtp', name: 'app-smtp', component: screens['app-smtp'], meta: { title: 'SMTP', section: 'smtp', nav: 'apps' } }
        ]
    };
}
