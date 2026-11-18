import { watch } from 'vue';

import { authState } from '@/auth';
import type { MessageSearch } from '@/message-search';

// Search terms are sensitive. They live only in this module's memory: never in browser storage, the URL,
// history state or the document title. A reload or a new tab starts empty by design.
const saved = new Map<string, MessageSearch>();

export function savedSearch(appId: string): MessageSearch | null {
    const entry = saved.get(appId);
    return entry ? { ...entry } : null;
}

export function saveSearch(appId: string, search: MessageSearch): void {
    if (!search.q) saved.delete(appId);
    else saved.set(appId, { q: search.q, field: search.field });
}

export function clearSavedSearches(): void {
    saved.clear();
}

watch(
    () => authState.status,
    status => {
        if (status !== 'signed-in') clearSavedSearches();
    }
);
