import { watch } from 'vue';

import { authState } from '@/auth';

// Suppression search terms are email addresses. They live only in this module's memory: never in browser
// storage, the URL, history state or the document title. A reload or a new tab starts empty by design.
const saved = new Map<string, string>();

export function savedSuppressionSearch(appId: string): string {
    return saved.get(appId) ?? '';
}

export function saveSuppressionSearch(appId: string, q: string): void {
    if (!q) saved.delete(appId);
    else saved.set(appId, q);
}

export function clearSavedSuppressionSearches(): void {
    saved.clear();
}

watch(
    () => authState.status,
    status => {
        if (status !== 'signed-in') clearSavedSuppressionSearches();
    }
);
