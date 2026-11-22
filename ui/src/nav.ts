/** Primary navigation entries; routes declare theirs through `meta.nav` (app-scoped routes belong to Apps). */
export type NavId = 'apps' | 'operations';

export interface NavItem {
    id: NavId;
    label: string;
    icon: string;
    path: string;
}

export const NAV: readonly NavItem[] = [
    { id: 'apps', label: 'Apps', icon: 'fa-solid fa-cube', path: '/apps' },
    { id: 'operations', label: 'Operations', icon: 'fa-solid fa-server', path: '/operations' }
];
