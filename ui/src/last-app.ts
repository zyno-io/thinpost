import { ref } from 'vue';

import { isOpaqueId } from './validation';

const KEY = 'thinpost:last-app';

function read(): string | null {
    try {
        const stored = localStorage.getItem(KEY);
        return isOpaqueId(stored) ? stored : null;
    } catch {
        return null;
    }
}

/** The remembered app ID (not secret); kept in sync with storage, including other tabs. */
export const lastAppId = ref<string | null>(read());

/** Re-reads storage so a router guard sees changes made since module load. */
export function rememberedAppId(): string | null {
    const id = read();
    lastAppId.value = id;
    return id;
}

export function rememberApp(id: string): void {
    if (!isOpaqueId(id)) return;
    lastAppId.value = id;
    try {
        localStorage.setItem(KEY, id);
    } catch {
        // storage unavailable; the in-memory value still applies for this page
    }
}

/** Forgets the remembered app only when it is the given one. */
export function forgetApp(id: string): void {
    if (read() !== id && lastAppId.value !== id) return;
    lastAppId.value = null;
    try {
        if (localStorage.getItem(KEY) === id) localStorage.removeItem(KEY);
    } catch {
        // storage unavailable
    }
}

window.addEventListener('storage', event => {
    if (event.key === KEY || event.key === null) lastAppId.value = read();
});
