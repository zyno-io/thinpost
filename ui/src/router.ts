import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router';

import { CALLBACK_PATH } from './auth';
import { rememberedAppId } from './last-app';
import { appRoute, type AppScreenName } from './routes';
import AppDomains from './screens/app-domains.vue';
import AppInbound from './screens/app-inbound.vue';
import InboundDetail from './screens/inbound-detail.vue';
import AppKeys from './screens/app-keys.vue';
import AppMessages from './screens/app-messages.vue';
import AppSuppressions from './screens/app-suppressions.vue';
import AppPicker from './screens/app-picker.vue';
import AppWebhooks from './screens/app-webhooks.vue';
import Apps from './screens/apps.vue';
import MessageDetail from './screens/message-detail.vue';
import NotFound from './screens/not-found.vue';
import AppTracking from './screens/app-tracking.vue';
import AppSmtp from './screens/app-smtp.vue';
import Operations from './screens/operations.vue';

/** Global links go straight to the remembered app's section; the picker remains when none is remembered. */
function redirectToRemembered(name: AppScreenName) {
    const appId = rememberedAppId();
    return appId ? { name, params: { appId } } : true;
}

const routes: RouteRecordRaw[] = [
    { path: '/', redirect: '/apps' },
    // initAuth captures the callback URL first; drop its code/state from the address bar.
    { path: CALLBACK_PATH, redirect: () => ({ path: '/', query: {}, hash: '' }) },
    { path: '/apps', name: 'apps', component: Apps, meta: { title: 'Apps', nav: 'apps' } },
    appRoute({
        'app-keys': AppKeys,
        'app-domains': AppDomains,
        'app-messages': AppMessages,
        'app-message': MessageDetail,
        'app-webhooks': AppWebhooks,
        'app-tracking': AppTracking,
        'app-suppressions': AppSuppressions,
        'app-inbound': AppInbound,
        'app-inbound-message': InboundDetail,
        'app-smtp': AppSmtp
    }),
    {
        path: '/domains',
        name: 'domains',
        component: AppPicker,
        props: {
            title: 'Sending domains',
            intro: 'Sending domains belong to an app. Pick an app to see and verify its domains.',
            target: 'app-domains'
        },
        meta: { title: 'Sending domains', nav: 'apps' },
        beforeEnter: () => redirectToRemembered('app-domains')
    },
    {
        path: '/messages',
        name: 'messages',
        component: AppPicker,
        props: { title: 'Messages', intro: 'Messages belong to an app. Pick an app to see what it has sent.', target: 'app-messages' },
        meta: { title: 'Messages', nav: 'apps' },
        beforeEnter: () => redirectToRemembered('app-messages')
    },
    {
        path: '/webhooks',
        name: 'webhooks',
        component: AppPicker,
        props: {
            title: 'Webhooks',
            intro: 'Webhooks belong to an app. Pick an app to manage where its delivery events are sent.',
            target: 'app-webhooks'
        },
        meta: { title: 'Webhooks', nav: 'apps' },
        beforeEnter: () => redirectToRemembered('app-webhooks')
    },
    {
        path: '/tracking',
        name: 'tracking',
        component: AppPicker,
        props: {
            title: 'Tracking',
            intro: 'Tracking belongs to an app. Pick an app to see its opens, clicks and tracking settings.',
            target: 'app-tracking'
        },
        meta: { title: 'Tracking', nav: 'apps' },
        beforeEnter: () => redirectToRemembered('app-tracking')
    },
    {
        path: '/inbound',
        name: 'inbound',
        component: AppPicker,
        props: {
            title: 'Inbound',
            intro: 'Receiving domains and inbound limits are managed per application.',
            target: 'app-inbound'
        },
        meta: { title: 'Inbound', nav: 'apps' },
        beforeEnter: () => redirectToRemembered('app-inbound')
    },
    { path: '/operations', name: 'operations', component: Operations, meta: { title: 'Operations', nav: 'operations' } },
    { path: '/:pathMatch(.*)*', name: 'not-found', component: NotFound, meta: { title: 'Not found' } }
];

const router = createRouter({
    history: createWebHistory(import.meta.env.BASE_URL),
    routes,
    scrollBehavior(to, from, saved) {
        if (saved) return saved;
        if (to.path === from.path) return false;
        return { top: 0 };
    }
});

router.afterEach(to => {
    const title = typeof to.meta.title === 'string' ? to.meta.title : null;
    document.title = title ? `${title} · Thinpost` : 'Thinpost';
});

export default router;
