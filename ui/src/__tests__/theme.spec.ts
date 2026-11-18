import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

type MediaListener = (event: { matches: boolean }) => void;

let listeners: MediaListener[] = [];
let systemDark = false;

function stubMatchMedia() {
    listeners = [];
    vi.stubGlobal('matchMedia', (query: string) => ({
        get matches() {
            return systemDark;
        },
        media: query,
        addEventListener: (_type: string, listener: MediaListener) => listeners.push(listener),
        removeEventListener: () => undefined
    }));
}

async function loadTheme() {
    vi.resetModules();
    return await import('@/theme');
}

beforeEach(() => {
    localStorage.clear();
    document.documentElement.classList.remove('dark');
    systemDark = false;
    stubMatchMedia();
});

afterEach(() => {
    vi.unstubAllGlobals();
});

describe('theme', () => {
    it('defaults to the system mode', async () => {
        const { useTheme } = await loadTheme();
        const { mode, isDark } = useTheme();
        expect(mode.value).toBe('system');
        expect(isDark.value).toBe(false);
        expect(document.documentElement.classList.contains('dark')).toBe(false);
    });

    it('persists an explicit dark choice under thinpost:theme and removes it for system', async () => {
        const { useTheme } = await loadTheme();
        const { mode, setMode } = useTheme();
        setMode('dark');
        expect(mode.value).toBe('dark');
        expect(localStorage.getItem('thinpost:theme')).toBe('dark');
        expect(document.documentElement.classList.contains('dark')).toBe(true);
        setMode('light');
        expect(localStorage.getItem('thinpost:theme')).toBe('light');
        expect(document.documentElement.classList.contains('dark')).toBe(false);
        setMode('system');
        expect(localStorage.getItem('thinpost:theme')).toBeNull();
    });

    it('reads a stored preference on load', async () => {
        localStorage.setItem('thinpost:theme', 'dark');
        const { useTheme } = await loadTheme();
        expect(useTheme().mode.value).toBe('dark');
        expect(document.documentElement.classList.contains('dark')).toBe(true);
    });

    it('follows a system preference change only in system mode', async () => {
        const { useTheme } = await loadTheme();
        const { setMode, isDark } = useTheme();
        for (const listener of listeners) listener({ matches: true });
        expect(isDark.value).toBe(true);
        expect(document.documentElement.classList.contains('dark')).toBe(true);
        setMode('light');
        for (const listener of listeners) listener({ matches: true });
        expect(isDark.value).toBe(false);
        expect(document.documentElement.classList.contains('dark')).toBe(false);
    });

    it('updates the mode when another tab changes the stored preference', async () => {
        const { useTheme } = await loadTheme();
        const { mode } = useTheme();
        localStorage.setItem('thinpost:theme', 'dark');
        window.dispatchEvent(new StorageEvent('storage', { key: 'thinpost:theme', newValue: 'dark' }));
        expect(mode.value).toBe('dark');
        expect(document.documentElement.classList.contains('dark')).toBe(true);
    });
});
