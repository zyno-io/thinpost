import { nextTick } from 'vue';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { authState } from '@/auth';
import { savedSearch, saveSearch } from '@/message-search-store';

describe('message search store', () => {
    beforeEach(() => {
        authState.status = 'signed-in';
    });
    afterEach(() => {
        vi.restoreAllMocks();
        authState.status = 'signed-in';
    });

    it('saves and restores per app', () => {
        saveSearch('a', { q: 'ada', field: 'recipient' });
        saveSearch('b', { q: 'x.example', field: 'sender_domain' });
        expect(savedSearch('a')).toEqual({ q: 'ada', field: 'recipient' });
        expect(savedSearch('b')).toEqual({ q: 'x.example', field: 'sender_domain' });
        expect(savedSearch('c')).toBeNull();
    });

    it('deletes the entry when the query is empty', () => {
        saveSearch('a', { q: 'ada', field: 'any' });
        saveSearch('a', { q: '', field: 'any' });
        expect(savedSearch('a')).toBeNull();
    });

    it('is cleared when the session leaves signed-in', async () => {
        saveSearch('a', { q: 'ada', field: 'any' });
        authState.status = 'ended';
        await nextTick();
        expect(savedSearch('a')).toBeNull();
    });

    it('never touches browser storage', () => {
        const set = vi.spyOn(Storage.prototype, 'setItem');
        saveSearch('a', { q: 'ada', field: 'any' });
        savedSearch('a');
        expect(set).not.toHaveBeenCalled();
    });
});
