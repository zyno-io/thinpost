import type { RouteLocationRaw } from 'vue-router';

import { isOpaqueId } from './validation';

/** Primary navigation entries; routes declare theirs through `meta.nav`. */
export type NavId = 'apps' | 'domains' | 'messages' | 'webhooks' | 'tracking' | 'inbound' | 'operations';

export interface NavItem {
    id: NavId;
    label: string;
    icon: string;
    /** Resolves the link; app-scoped entries go to the given app when there is one. */
    target(appId: string | null): RouteLocationRaw;
}

function appSection(id: 'domains' | 'messages' | 'webhooks' | 'tracking' | 'inbound', label: string, icon: string): NavItem {
    return {
        id,
        label,
        icon,
        target: appId => (appId && isOpaqueId(appId) ? { name: `app-${id}`, params: { appId } } : `/${id}`)
    };
}

export const NAV: readonly NavItem[] = [
    { id: 'apps', label: 'Apps', icon: 'fa-solid fa-cube', target: () => '/apps' },
    appSection('domains', 'Sending domains', 'fa-solid fa-globe'),
    appSection('messages', 'Messages', 'fa-solid fa-envelope'),
    appSection('webhooks', 'Webhooks', 'fa-solid fa-bolt'),
    appSection('tracking', 'Tracking', 'fa-solid fa-chart-line'),
    appSection('inbound', 'Inbound', 'fa-solid fa-inbox'),
    { id: 'operations', label: 'Operations', icon: 'fa-solid fa-server', target: () => '/operations' }
];
