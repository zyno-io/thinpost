import { flushPromises, type VueWrapper } from '@vue/test-utils';
import type { Router } from 'vue-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppSuppressions: vi.fn(),
    searchAppSuppressions: vi.fn(),
    addAppSuppressions: vi.fn(),
    deleteAppSuppressions: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showConfirmDestroy: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { clearSavedSuppressionSearches, saveSuppressionSearch } = await import('@/suppression-search-store');
const { default: AppSuppressions } = await import('@/screens/app-suppressions.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const MESSAGE_ID = 'abababababababababababababababab';

function row(n: number, overrides: Record<string, unknown> = {}) {
    return {
        id: n.toString(16).padStart(32, '0'),
        address: `user${n}@example.com`,
        reason: 'manual',
        origin: 'console',
        created_at: '2026-10-02T12:00:00Z',
        ...overrides
    };
}

let wrapper: VueWrapper | undefined;
let router: Router;

async function mountAt(path = `/apps/${APP_ID}/suppressions`) {
    const mounted = await mountAppRoute(path, { 'app-suppressions': AppSuppressions });
    wrapper = mounted.wrapper;
    router = mounted.router;
    return wrapper;
}

function button(label: string) {
    const match = wrapper!.findAll('button').find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}

const rows = () => wrapper!.findAll('tbody tr');

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    vi.useRealTimers();
    clearSavedSuppressionSearches();
    authState.status = 'signed-in';
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.listAppSuppressions.mockResolvedValue({ data: { suppressions: [row(1), row(2, { reason: 'hard_bounce', origin: 'recipient', source_message_id: MESSAGE_ID })] } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
    document.body.innerHTML = '';
});

describe('suppressions screen', () => {
    it('lists entries with labels and a source link', async () => {
        await mountAt();
        expect(api.listAppSuppressions).toHaveBeenCalledWith({ path: { appId: APP_ID }, query: { limit: 50 } });
        expect(rows()).toHaveLength(2);
        const text = wrapper!.text();
        expect(text).toContain('user1@example.com');
        expect(text).toContain('Bounced');
        expect(text).toContain('Added manually');
        expect(text).toContain('Recipient');
        expect(text).toContain('Console');
        const link = rows()[1]!.find('a');
        expect(link.attributes('href')).toBe(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(rows()[0]!.find('a').exists()).toBe(false);
    });

    it('shows the help text', async () => {
        await mountAt();
        const help = await openHelp(wrapper!, 'suppression list');
        expect(help).toContain('Thinpost checks this list before every send');
        expect(help).toContain('Messages already skipped are not sent later');
        expect(help).toContain("Thinpost adds an address on its own only when the recipient's mail server");
    });

    it('shows empty and error states', async () => {
        api.listAppSuppressions.mockResolvedValue({ data: { suppressions: [] } });
        await mountAt();
        expect(wrapper!.text()).toContain('No suppressed addresses');
        wrapper!.unmount();
        api.listAppSuppressions.mockRejectedValue(httpError(503));
        await mountAt();
        expect(wrapper!.text()).toContain('unavailable');
        expect(wrapper!.find('table').exists()).toBe(false);
    });

    it('pages with Load more and appends without duplicates', async () => {
        api.listAppSuppressions.mockResolvedValueOnce({ data: { suppressions: [row(1), row(2)], next_cursor: 'c1' } });
        api.listAppSuppressions.mockResolvedValueOnce({ data: { suppressions: [row(2), row(3)] } });
        await mountAt();
        await button('Load more').trigger('click');
        await flushPromises();
        expect(api.listAppSuppressions).toHaveBeenLastCalledWith({ path: { appId: APP_ID }, query: { limit: 50, cursor: 'c1' } });
        expect(rows()).toHaveLength(3);
        expect(wrapper!.text()).not.toContain('Load more');
    });

    it('filters by reason through the list endpoint', async () => {
        await mountAt();
        await wrapper!.find('select[aria-label="Reason"]').setValue('hard_bounce');
        await flushPromises();
        expect(api.listAppSuppressions).toHaveBeenLastCalledWith({ path: { appId: APP_ID }, query: { limit: 50, reason: 'hard_bounce' } });
    });

    it('searches by prefix and by exact domain, with terms only in POST bodies', async () => {
        api.searchAppSuppressions.mockResolvedValue({ data: { suppressions: [row(1)] } });
        await mountAt();
        const startPath = router.currentRoute.value.fullPath;
        const input = wrapper!.find('input[type="search"]');
        await input.setValue('user1@');
        await wrapper!.find('form[role="search"]').trigger('submit');
        await flushPromises();
        expect(api.searchAppSuppressions).toHaveBeenLastCalledWith(expect.objectContaining({ path: { appId: APP_ID }, body: { q: 'user1@', field: 'address', limit: 50 } }));
        await input.setValue('@example.com');
        await wrapper!.find('form[role="search"]').trigger('submit');
        await flushPromises();
        expect(api.searchAppSuppressions).toHaveBeenLastCalledWith(expect.objectContaining({ body: { q: 'example.com', field: 'domain', limit: 50 } }));

        // Never in the URL, history state, storage or toasts.
        expect(router.currentRoute.value.fullPath).toBe(startPath);
        expect(JSON.stringify(window.history.state)).not.toContain('example.com');
        expect(JSON.stringify({ ...localStorage, ...sessionStorage })).not.toContain('example.com');
        expect(vf.showToast).not.toHaveBeenCalled();
        expect(api.listAppSuppressions.mock.calls.every(c => !JSON.stringify(c).includes('user1@'))).toBe(true);
    });

    it('starts with a search handed over in memory', async () => {
        saveSuppressionSearch(APP_ID, 'user1@example.com');
        api.searchAppSuppressions.mockResolvedValue({ data: { suppressions: [row(1)] } });
        await mountAt();
        expect((wrapper!.find('input[type="search"]').element as HTMLInputElement).value).toBe('user1@example.com');
        expect(api.searchAppSuppressions).toHaveBeenCalledWith(expect.objectContaining({ body: { q: 'user1@example.com', field: 'address', limit: 50 } }));
        expect(api.listAppSuppressions).not.toHaveBeenCalled();
    });

    describe('add', () => {
        async function openAdd() {
            await button('Add').trigger('click');
            await flushPromises();
            return document.querySelector('textarea') as HTMLTextAreaElement;
        }

        it('shows valid and invalid counts and blocks invalid input', async () => {
            await mountAt();
            const area = await openAdd();
            area.value = 'a@example.com\nbad\nb@example.com';
            area.dispatchEvent(new Event('input'));
            await flushPromises();
            expect(document.querySelector('[role="status"].count')!.textContent).toContain('2 valid · 1 invalid');
            const submit = [...document.querySelectorAll('[role="dialog"] button')].find(b => b.textContent?.trim() === 'Add') as HTMLButtonElement;
            expect(submit.disabled).toBe(true);
            area.value = 'a@example.com';
            area.dispatchEvent(new Event('input'));
            await flushPromises();
            expect(submit.disabled).toBe(false);
        });

        it('rejects more than 100 addresses', async () => {
            await mountAt();
            const area = await openAdd();
            area.value = Array.from({ length: 101 }, (_, i) => `u${i}@example.com`).join('\n');
            area.dispatchEvent(new Event('input'));
            await flushPromises();
            expect(document.querySelector('.count')!.textContent).toContain('over the 100 limit');
            expect((([...document.querySelectorAll('[role="dialog"] button')].find(b => b.textContent?.trim() === 'Add')) as HTMLButtonElement).disabled).toBe(true);
        });

        it('submits and reports added and already suppressed counts', async () => {
            api.addAppSuppressions.mockResolvedValue({
                data: {
                    results: [
                        { address: 'a@example.com', status: 'added', suppression: row(5) },
                        { address: 'b@example.com', status: 'exists', suppression: row(6) },
                        { address: 'c@example.com', status: 'exists', suppression: row(7) }
                    ]
                }
            });
            await mountAt();
            const area = await openAdd();
            area.value = 'a@example.com\nb@example.com\nc@example.com';
            area.dispatchEvent(new Event('input'));
            await flushPromises();
            const submit = [...document.querySelectorAll('[role="dialog"] button')].find(b => b.textContent?.trim() === 'Add') as HTMLButtonElement;
            submit.click();
            await flushPromises();
            expect(api.addAppSuppressions).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { addresses: ['a@example.com', 'b@example.com', 'c@example.com'] } });
            expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Added 1 · already on the list 2' }));
            expect(document.querySelector('textarea')).toBeNull();
            expect(api.listAppSuppressions).toHaveBeenCalledTimes(2);
        });
    });

    describe('remove', () => {
        it('confirms a single removal with the short text and reloads', async () => {
            vf.showConfirmDestroy.mockResolvedValue(true);
            api.deleteAppSuppressions.mockResolvedValue({ data: { deleted: 1 } });
            await mountAt();
            await rows()[0]!.findAll('button').find(b => b.text().includes('Remove'))!.trigger('click');
            await flushPromises();
            expect(vf.showConfirmDestroy).toHaveBeenCalledWith(expect.any(String), 'Thinpost will send to these addresses again. Messages already skipped are not sent later.');
            expect(api.deleteAppSuppressions).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { ids: [row(1).id] } });
            expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Removed 1' }));
            expect(api.listAppSuppressions).toHaveBeenCalledTimes(2);
        });

        it('asks for the stronger confirmation before removing a recipient-requested entry', async () => {
            vf.showConfirmDestroy.mockResolvedValue(true);
            api.deleteAppSuppressions.mockResolvedValue({ data: { deleted: 1 } });
            api.listAppSuppressions.mockResolvedValue({
                data: { suppressions: [row(1, { reason: 'unsubscribe', origin: 'recipient' }), row(2, { reason: 'hard_bounce', origin: 'recipient' })] }
            });
            await mountAt();
            await rows()[0]!.findAll('button').find(b => b.text().includes('Remove'))!.trigger('click');
            await flushPromises();
            expect(vf.showConfirmDestroy).toHaveBeenCalledWith(expect.any(String), expect.stringContaining('The recipient asked not to receive this email. Remove anyway?'));
            vf.showConfirmDestroy.mockClear();
            await rows()[1]!.findAll('button').find(b => b.text().includes('Remove'))!.trigger('click');
            await flushPromises();
            expect(vf.showConfirmDestroy).toHaveBeenCalledWith(expect.any(String), 'Thinpost will send to these addresses again. Messages already skipped are not sent later.');
        });

        it('names how many selected entries were requested by the recipient', async () => {
            vf.showConfirmDestroy.mockResolvedValue(true);
            api.deleteAppSuppressions.mockResolvedValue({ data: { deleted: 2 } });
            api.listAppSuppressions.mockResolvedValue({
                data: { suppressions: [row(1, { reason: 'spam_complaint', origin: 'recipient' }), row(2)] }
            });
            await mountAt();
            await wrapper!.find('thead input[type="checkbox"]').setValue(true);
            await button('Remove 2').trigger('click');
            await flushPromises();
            expect(vf.showConfirmDestroy).toHaveBeenCalledWith(expect.stringContaining('2'), expect.stringContaining('One recipient asked not to receive this email. Remove anyway?'));
        });

        it('does nothing when the confirmation is declined', async () => {
            vf.showConfirmDestroy.mockResolvedValue(false);
            await mountAt();
            await button('Remove').trigger('click');
            await flushPromises();
            expect(api.deleteAppSuppressions).not.toHaveBeenCalled();
        });

        it('removes the selection in bulk', async () => {
            vf.showConfirmDestroy.mockResolvedValue(true);
            api.deleteAppSuppressions.mockResolvedValue({ data: { deleted: 2 } });
            await mountAt();
            expect(wrapper!.text()).not.toContain('Remove 2');
            await wrapper!.find('thead input[type="checkbox"]').setValue(true);
            expect(button('Remove 2')).toBeTruthy();
            await button('Remove 2').trigger('click');
            await flushPromises();
            expect(vf.showConfirmDestroy).toHaveBeenCalledWith(expect.stringContaining('2'), expect.stringContaining('Messages already skipped are not sent later.'));
            expect(api.deleteAppSuppressions).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { ids: [row(1).id, row(2).id] } });
        });

        it('reloads the list after an ambiguous failure', async () => {
            vf.showConfirmDestroy.mockResolvedValue(true);
            api.deleteAppSuppressions.mockRejectedValue(httpError(502));
            await mountAt();
            await rows()[0]!.findAll('button').find(b => b.text().includes('Remove'))!.trigger('click');
            await flushPromises();
            expect(wrapper!.text()).toContain("We can't tell whether the change went through");
            expect(api.listAppSuppressions).toHaveBeenCalledTimes(2);
        });
    });

    it('disables actions when signed out', async () => {
        await mountAt();
        authState.status = 'signed-out';
        await flushPromises();
        expect(button('Add').attributes('disabled')).toBeDefined();
    });

    describe('review fixes', () => {
        const page = (from: number, n: number) => Array.from({ length: n }, (_, i) => row(from + i));
        const removeButton = (n: number) => rows()[n]!.findAll('button').find(b => b.text().includes('Remove'))!;

        it('removes more than 100 selected rows in batches of 100 and sums the result', async () => {
            api.listAppSuppressions
                .mockResolvedValueOnce({ data: { suppressions: page(1, 50), next_cursor: 'c1' } })
                .mockResolvedValueOnce({ data: { suppressions: page(51, 50), next_cursor: 'c2' } })
                .mockResolvedValueOnce({ data: { suppressions: page(101, 50) } })
                .mockResolvedValue({ data: { suppressions: [] } });
            vf.showConfirmDestroy.mockResolvedValue(true);
            api.deleteAppSuppressions.mockResolvedValueOnce({ data: { deleted: 100 } }).mockResolvedValueOnce({ data: { deleted: 50 } });
            await mountAt();
            for (let i = 0; i < 2; i++) {
                await button('Load more').trigger('click');
                await flushPromises();
            }
            await wrapper!.find('thead input[type="checkbox"]').setValue(true);
            await button('Remove 150').trigger('click');
            await flushPromises();
            const sizes = api.deleteAppSuppressions.mock.calls.map(c => c[0].body.ids.length);
            expect(sizes).toEqual([100, 50]);
            expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Removed 150' }));
        });

        it('treats a failed later batch as ambiguous: reports progress, shows the error and reloads', async () => {
            api.listAppSuppressions
                .mockResolvedValueOnce({ data: { suppressions: page(1, 50), next_cursor: 'c1' } })
                .mockResolvedValueOnce({ data: { suppressions: page(51, 60) } })
                .mockResolvedValue({ data: { suppressions: page(101, 2) } });
            vf.showConfirmDestroy.mockResolvedValue(true);
            api.deleteAppSuppressions.mockResolvedValueOnce({ data: { deleted: 100 } }).mockRejectedValueOnce(httpError(400));
            await mountAt();
            await button('Load more').trigger('click');
            await flushPromises();
            await wrapper!.find('thead input[type="checkbox"]').setValue(true);
            await button('Remove 110').trigger('click');
            await flushPromises();
            expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Removed 100' }));
            expect(wrapper!.text()).toContain('rejected the request');
            expect(api.listAppSuppressions).toHaveBeenCalledTimes(3);
            expect(rows()).toHaveLength(2);
        });

        it('does not leave the loader stuck when a refresh supersedes a filter load', async () => {
            api.listAppSuppressions.mockResolvedValueOnce({ data: { suppressions: page(1, 3) } });
            await mountAt();
            vf.showConfirmDestroy.mockResolvedValue(true);
            let resolveDelete!: (v: unknown) => void;
            api.deleteAppSuppressions.mockReturnValue(new Promise(r => (resolveDelete = r)));
            let resolveLoad!: (v: unknown) => void;
            api.listAppSuppressions.mockReturnValueOnce(new Promise(r => (resolveLoad = r)));
            api.listAppSuppressions.mockResolvedValueOnce({ data: { suppressions: page(2, 2) } });
            await removeButton(0).trigger('click');
            await flushPromises();
            await wrapper!.find('select[aria-label="Reason"]').setValue('manual');
            await flushPromises();
            resolveDelete({ data: { deleted: 1 } });
            await flushPromises();
            resolveLoad({ data: { suppressions: page(1, 3) } });
            await flushPromises();
            expect(wrapper!.text()).not.toContain('Loading suppressions');
            expect(rows()).toHaveLength(2);
            expect(wrapper!.text()).not.toContain('user1@example.com');
        });

        it('disables row actions while a refresh runs, and a post-delete reload supersedes an older one', async () => {
            api.listAppSuppressions.mockResolvedValueOnce({ data: { suppressions: page(1, 2) } });
            await mountAt();
            let resolveRefresh!: (v: unknown) => void;
            api.listAppSuppressions.mockReturnValueOnce(new Promise(r => (resolveRefresh = r)));
            await button('Refresh').trigger('click');
            expect(removeButton(0).attributes('disabled')).toBeDefined();
            expect(button('Add').attributes('disabled')).toBeDefined();
            resolveRefresh({ data: { suppressions: page(1, 2) } });
            await flushPromises();
            expect(removeButton(0).attributes('disabled')).toBeUndefined();
        });

        it('reloads after an ambiguous add failure and keeps the modal open', async () => {
            api.addAppSuppressions.mockRejectedValue(httpError(502));
            await mountAt();
            await button('Add').trigger('click');
            await flushPromises();
            const area = document.querySelector('textarea')!;
            area.value = 'a@example.com';
            area.dispatchEvent(new Event('input'));
            await flushPromises();
            ([...document.querySelectorAll('[role="dialog"] button')].find(b => b.textContent?.trim() === 'Add') as HTMLButtonElement).click();
            await flushPromises();
            expect(document.querySelector('textarea')).not.toBeNull();
            expect(document.body.textContent).toContain("We can't tell whether the change went through");
            expect(api.listAppSuppressions).toHaveBeenCalledTimes(2);
        });

        it('shows a short hint when the server rejects an address with 400', async () => {
            api.addAppSuppressions.mockRejectedValue(httpError(400));
            await mountAt();
            await button('Add').trigger('click');
            await flushPromises();
            const area = document.querySelector('textarea')!;
            area.value = 'a@example.com';
            area.dispatchEvent(new Event('input'));
            await flushPromises();
            ([...document.querySelectorAll('[role="dialog"] button')].find(b => b.textContent?.trim() === 'Add') as HTMLButtonElement).click();
            await flushPromises();
            expect(document.body.textContent).toContain('Thinpost rejected one or more of the addresses.');
        });

        it('combines search and reason in one search request', async () => {
            api.searchAppSuppressions.mockResolvedValue({ data: { suppressions: [row(1)] } });
            await mountAt();
            await wrapper!.find('input[type="search"]').setValue('@example.com');
            await wrapper!.find('form[role="search"]').trigger('submit');
            await flushPromises();
            await wrapper!.find('select[aria-label="Reason"]').setValue('hard_bounce');
            await flushPromises();
            expect(api.searchAppSuppressions).toHaveBeenLastCalledWith(
                expect.objectContaining({ body: { q: 'example.com', field: 'domain', reason: 'hard_bounce', limit: 50 } })
            );
        });
    });
});
