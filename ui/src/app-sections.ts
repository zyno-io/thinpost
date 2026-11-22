export type AppSection = 'keys' | 'domains' | 'messages' | 'webhooks' | 'tracking' | 'suppressions' | 'inbound' | 'smtp';

export interface AppSectionInfo {
    id: AppSection;
    /** Name of the section's route. */
    route: 'app-keys' | 'app-domains' | 'app-messages' | 'app-webhooks' | 'app-tracking' | 'app-suppressions' | 'app-inbound' | 'app-smtp';
    /** Shown only when the server reports this runtime feature. */
    feature?: 'smtp_submission';
    label: string;
    icon: string;
}

/** Sections of an app, in tab order. */
export const APP_SECTIONS: readonly AppSectionInfo[] = [
    { id: 'keys', route: 'app-keys', label: 'API keys', icon: 'fa-solid fa-key' },
    { id: 'smtp', route: 'app-smtp', feature: 'smtp_submission', label: 'SMTP', icon: 'fa-solid fa-paper-plane' },
    { id: 'domains', route: 'app-domains', label: 'Sending domains', icon: 'fa-solid fa-globe' },
    { id: 'messages', route: 'app-messages', label: 'Messages', icon: 'fa-solid fa-envelope' },
    { id: 'webhooks', route: 'app-webhooks', label: 'Webhooks', icon: 'fa-solid fa-bolt' },
    { id: 'tracking', route: 'app-tracking', label: 'Tracking', icon: 'fa-solid fa-chart-line' },
    { id: 'suppressions', route: 'app-suppressions', label: 'Suppressions', icon: 'fa-solid fa-ban' },
    { id: 'inbound', route: 'app-inbound', label: 'Inbound', icon: 'fa-solid fa-inbox' }
];

export function sectionInfo(id: AppSection | undefined): AppSectionInfo | undefined {
    return APP_SECTIONS.find(section => section.id === id);
}
