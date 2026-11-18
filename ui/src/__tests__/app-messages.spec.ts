import { flushPromises, type VueWrapper } from '@vue/test-utils';
import type { Router } from 'vue-router';
import { nextTick } from 'vue';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppMessages: vi.fn(),
    searchAppMessages: vi.fn(),
    getAppMessageSubject: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { clearSavedSearches } = await import('@/message-search-store');
const { default: AppMessages } = await import('@/screens/app-messages.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };

function message(n: number, overrides: Record<string, unknown> = {}) {
    return {
        id: n.toString(16).padStart(32, '0'),
        state: 'prepared',
        status: 'delivered',
        enhanced_protection: false,
        subject_masked: false,
        sender_domain: 'example.com',
        accepted_at: '2026-10-02T12:00:00Z',
        delivery_count: 2,
        accepted_count: 1,
        failed_count: 0,
        uncertain_count: 0,
        ...overrides
    };
}

let wrapper: VueWrapper | undefined;
let router: Router;

async function mountAt(path: string) {
    const mounted = await mountAppRoute(path, { 'app-messages': AppMessages });
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
    clearSavedSearches();
    authState.status = 'signed-in';
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('messages screen', () => {
    it('pages through history with the server cursor', async () => {
        api.listAppMessages
            .mockResolvedValueOnce({ data: { messages: [message(3), message(2)], next_cursor: 'cursor-1' } })
            .mockResolvedValueOnce({ data: { messages: [message(1)] } });

        await mountAt(`/apps/${APP_ID}/messages`);
        expect(api.listAppMessages).toHaveBeenLastCalledWith({ path: { appId: APP_ID }, query: { limit: 50 } });
        expect(rows()).toHaveLength(2);

        await button('Load older messages').trigger('click');
        await flushPromises();
        expect(api.listAppMessages).toHaveBeenLastCalledWith({ path: { appId: APP_ID }, query: { limit: 50, cursor: 'cursor-1' } });
        expect(rows()).toHaveLength(3);
        expect(wrapper!.text()).toContain('end of history');
        expect(wrapper!.findAll('button').some(b => b.text().includes('Load older'))).toBe(false);
    });

    it('keeps loaded rows and retries the same cursor after a failed page', async () => {
        api.listAppMessages
            .mockResolvedValueOnce({ data: { messages: [message(2)], next_cursor: 'cursor-1' } })
            .mockRejectedValueOnce(httpError(503))
            .mockResolvedValueOnce({ data: { messages: [message(1)] } });

        await mountAt(`/apps/${APP_ID}/messages`);
        await button('Load older messages').trigger('click');
        await flushPromises();
        expect(rows()).toHaveLength(1);
        expect(wrapper!.text()).toContain('temporarily unavailable');

        await button('Try again').trigger('click');
        await flushPromises();
        expect(api.listAppMessages).toHaveBeenLastCalledWith({ path: { appId: APP_ID }, query: { limit: 50, cursor: 'cursor-1' } });
        expect(rows()).toHaveLength(2);
    });

    it('refresh restarts from the first page', async () => {
        api.listAppMessages
            .mockResolvedValueOnce({ data: { messages: [message(2)], next_cursor: 'cursor-1' } })
            .mockResolvedValueOnce({ data: { messages: [message(1)] } })
            .mockResolvedValueOnce({ data: { messages: [message(3)], next_cursor: 'cursor-2' } });

        await mountAt(`/apps/${APP_ID}/messages`);
        await button('Load older messages').trigger('click');
        await flushPromises();
        await button('Refresh').trigger('click');
        await flushPromises();
        expect(api.listAppMessages).toHaveBeenLastCalledWith({ path: { appId: APP_ID }, query: { limit: 50 } });
        expect(rows()).toHaveLength(1);
        expect(button('Load older messages').exists()).toBe(true);
    });

    it('refresh after a failed first load replaces the error with rows', async () => {
        api.listAppMessages.mockRejectedValueOnce(httpError(503)).mockResolvedValueOnce({ data: { messages: [message(1)] } });
        await mountAt(`/apps/${APP_ID}/messages`);
        expect(wrapper!.find('.api-error').exists()).toBe(true);
        await button('Refresh').trigger('click');
        await flushPromises();
        expect(wrapper!.find('.api-error').exists()).toBe(false);
        expect(wrapper!.text()).not.toContain('from the previous load');
        expect(rows()).toHaveLength(1);
    });

    it('shows delivery counts, status tags and links to detail', async () => {
        api.listAppMessages.mockResolvedValue({ data: { messages: [message(1, { failed_count: 1, uncertain_count: 1 })] } });
        await mountAt(`/apps/${APP_ID}/messages`);
        const row = rows()[0]!;
        expect(row.text()).toContain('1/2');
        expect(row.text()).toContain('1 failed');
        expect(row.text()).toContain('1 uncertain');
        expect(row.text()).toContain('Delivered');
        expect(row.find('a').attributes('href')).toBe(`/apps/${APP_ID}/messages/${message(1).id}`);
        expect(row.find('a').attributes('title')).toBe(message(1).id);
        // Explanatory copy lives behind the help button, not on the page.
        expect(wrapper!.text()).not.toContain('Enhanced Protection is chosen for each message');
        expect(wrapper!.text()).not.toMatch(/audit/i);
        expect(wrapper!.text()).not.toContain('TLS');
        expect(await openHelp(wrapper!, 'message search')).toContain('Enhanced Protection is chosen for each message');
    });

    describe('Enhanced Protection subjects', () => {
        const ep = (n: number, extra: Record<string, unknown> = {}) =>
            message(n, { enhanced_protection: true, subject_masked: true, subject: 'LEAK', ...extra });

        it('masks the subject and offers a reveal', async () => {
            api.listAppMessages.mockResolvedValue({ data: { messages: [ep(1)] } });
            await mountAt(`/apps/${APP_ID}/messages`);
            const row = rows()[0]!;
            expect(row.text()).toContain('Enhanced Protection');
            expect(row.text()).toContain('Protected subject');
            expect(row.text()).not.toContain('LEAK');
            expect(row.findAll('button').some(b => b.text().includes('Reveal'))).toBe(true);
        });

        it('does not call the API when the confirmation is declined', async () => {
            api.listAppMessages.mockResolvedValue({ data: { messages: [ep(1)] } });
            vf.showConfirm.mockResolvedValue(false);
            await mountAt(`/apps/${APP_ID}/messages`);
            await button('Reveal').trigger('click');
            await flushPromises();
            expect(vf.showConfirm.mock.calls[0]![1]).toContain('audit log');
            expect(api.getAppMessageSubject).not.toHaveBeenCalled();
        });

        it('reveals after confirmation, hides, and clears on refresh', async () => {
            api.listAppMessages.mockResolvedValue({ data: { messages: [ep(1)] } });
            api.getAppMessageSubject.mockResolvedValue({ data: { subject: 'Invoice 42' } });
            vf.showConfirm.mockResolvedValue(true);
            await mountAt(`/apps/${APP_ID}/messages`);
            await button('Reveal').trigger('click');
            await flushPromises();
            expect(api.getAppMessageSubject).toHaveBeenCalledWith({ path: { appId: APP_ID, messageId: message(1).id } });
            expect(rows()[0]!.text()).toContain('Invoice 42');
            expect(rows()[0]!.text()).toContain('Revealed');

            await button('Hide').trigger('click');
            expect(rows()[0]!.text()).not.toContain('Invoice 42');

            await button('Reveal').trigger('click');
            await flushPromises();
            expect(rows()[0]!.text()).toContain('Invoice 42');
            await button('Refresh').trigger('click');
            await flushPromises();
            expect(rows()[0]!.text()).not.toContain('Invoice 42');
        });

        it('clears revealed subjects when the session ends', async () => {
            api.listAppMessages.mockResolvedValue({ data: { messages: [ep(1)] } });
            api.getAppMessageSubject.mockResolvedValue({ data: { subject: 'Invoice 42' } });
            vf.showConfirm.mockResolvedValue(true);
            await mountAt(`/apps/${APP_ID}/messages`);
            await button('Reveal').trigger('click');
            await flushPromises();
            authState.status = 'ended';
            await flushPromises();
            expect(wrapper!.text()).not.toContain('Invoice 42');
            expect(button('Reveal').attributes('disabled')).toBeDefined();
        });

        it('shows 409 as no subject recorded', async () => {
            api.listAppMessages.mockResolvedValue({ data: { messages: [ep(1)] } });
            api.getAppMessageSubject.mockRejectedValue(httpError(409));
            vf.showConfirm.mockResolvedValue(true);
            await mountAt(`/apps/${APP_ID}/messages`);
            await button('Reveal').trigger('click');
            await flushPromises();
            expect(rows()[0]!.text()).toContain('No subject recorded');
        });

        it('explains a 503 and notes the audit attempt', async () => {
            api.listAppMessages.mockResolvedValue({ data: { messages: [ep(1)] } });
            api.getAppMessageSubject.mockRejectedValue(httpError(503));
            vf.showConfirm.mockResolvedValue(true);
            await mountAt(`/apps/${APP_ID}/messages`);
            await button('Reveal').trigger('click');
            await flushPromises();
            expect(wrapper!.text()).toContain('Revealing the subject is temporarily unavailable');
            expect(wrapper!.text()).toContain('may still have been recorded in the audit log');
        });

        it('shows non-EP subjects without a reveal or dialog', async () => {
            api.listAppMessages.mockResolvedValue({ data: { messages: [message(1, { subject: 'Plain subject' })] } });
            await mountAt(`/apps/${APP_ID}/messages`);
            expect(rows()[0]!.text()).toContain('Plain subject');
            expect(wrapper!.findAll('button').some(b => b.text().includes('Reveal'))).toBe(false);
            expect(vf.showConfirm).not.toHaveBeenCalled();
        });
    });

    it('shows "Not recorded" for a missing subject and recipients and a +N tag from recipient_count', async () => {
        api.listAppMessages.mockResolvedValue({
            data: {
                messages: [message(2), message(1, { subject: 'Hi', recipients: ['ada@example.org', 'grace@example.net'], recipient_count: 5 })]
            }
        });
        await mountAt(`/apps/${APP_ID}/messages`);
        const first = rows()[0]!.findAll('td');
        expect(first[2]!.text()).toContain('Not recorded'); // To
        expect(first[3]!.text()).toContain('Not recorded'); // Subject
        const second = rows()[1]!;
        expect(second.text()).toContain('ada@example.org');
        expect(second.text()).toContain('+4');
    });

    it('keeps Cc and Bcc recipients out of the To column, its tooltip and its +N', async () => {
        api.listAppMessages.mockResolvedValue({
            data: {
                messages: [
                    message(2, { subject: 'A', recipients: ['to@example.org', 'cc@example.net', 'bcc@example.com'], recipient_kinds: ['to', 'cc', 'bcc'], recipient_count: 3 }),
                    message(1, { subject: 'B', recipients: ['to1@example.org', 'to2@example.org', 'cc@example.net'], recipient_kinds: ['to', 'to', 'cc'], recipient_count: 3 })
                ]
            }
        });
        await mountAt(`/apps/${APP_ID}/messages`);
        const [one, two] = rows();
        expect(one!.findAll('td')[2]!.text()).toContain('to@example.org');
        expect(one!.findAll('td')[2]!.text()).not.toContain('+');
        expect(one!.find('.addr').attributes('title')).toBe('to@example.org');
        expect(two!.findAll('td')[2]!.text()).toContain('+1');
        expect(two!.find('.addr').attributes('title')).toBe('to1@example.org, to2@example.org');
    });

    it('shows the sender domain for protected and plain rows and "Not recorded" when absent', async () => {
        api.listAppMessages.mockResolvedValue({
            data: {
                messages: [
                    message(3, { enhanced_protection: true, subject_masked: true, sender_domain: 'secret.example' }),
                    message(2),
                    message(1, { sender_domain: undefined })
                ]
            }
        });
        await mountAt(`/apps/${APP_ID}/messages`);
        expect(wrapper!.find('thead').text()).toContain('Sender domain');
        const cells = rows().map(r => r.findAll('td')[1]!.text());
        expect(cells).toEqual(['secret.example', 'example.com', 'Not recorded']);
    });

    describe('domain shortcuts', () => {
        it('searches by sender domain in memory only', async () => {
            const setItem = vi.spyOn(Storage.prototype, 'setItem');
            api.listAppMessages.mockResolvedValue({ data: { messages: [message(1)] } });
            api.searchAppMessages.mockResolvedValue({ data: { messages: [message(1)] } });
            await mountAt(`/apps/${APP_ID}/messages`);
            const before = router.currentRoute.value.fullPath;
            await wrapper!.find('button.domain-filter').trigger('click');
            await flushPromises();
            expect(api.searchAppMessages).toHaveBeenLastCalledWith({
                path: { appId: APP_ID },
                body: { q: 'example.com', field: 'sender_domain', limit: 50 },
                signal: expect.any(AbortSignal)
            });
            const select = wrapper!.find('select[aria-label="Search field"]').element as HTMLSelectElement;
            expect(select.selectedOptions[0]!.text).toBe('Sender domain');
            const input = wrapper!.find('input[type="search"]');
            expect((input.element as HTMLInputElement).value).toBe('example.com');
            expect(document.activeElement).toBe(input.element);
            expect(router.currentRoute.value.fullPath).toBe(before);
            expect(setItem).not.toHaveBeenCalled();
            setItem.mockRestore();
        });

        it('searches by recipient domain from the To cell', async () => {
            api.listAppMessages.mockResolvedValue({ data: { messages: [message(1, { recipients: ['ada@Mail.Example.org'] })] } });
            api.searchAppMessages.mockResolvedValue({ data: { messages: [] } });
            await mountAt(`/apps/${APP_ID}/messages`);
            expect(rows()[0]!.findAll('td')[2]!.text()).toContain('ada@Mail.Example.org');
            await wrapper!.findAll('button.domain-filter')[1]!.trigger('click');
            await flushPromises();
            expect(api.searchAppMessages).toHaveBeenLastCalledWith(
                expect.objectContaining({ body: { q: 'mail.example.org', field: 'recipient_domain', limit: 50 } })
            );
        });
    });

    describe('search', () => {
        beforeEach(() => {
            api.listAppMessages.mockResolvedValue({ data: { messages: [message(1)] } });
            api.searchAppMessages.mockResolvedValue({ data: { messages: [message(2)] } });
        });

        const input = () => wrapper!.find('input[type="search"]');
        const field = () => wrapper!.find('select[aria-label="Search field"]');

        async function type(value: string) {
            await input().setValue(value);
        }

        it('debounces typing and keeps terms out of the URL, storage and history', async () => {
            const setItem = vi.spyOn(Storage.prototype, 'setItem');
            await mountAt(`/apps/${APP_ID}/messages`);
            vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
            const path = router.currentRoute.value.fullPath;
            await type('ada');
            await vi.advanceTimersByTimeAsync(299);
            expect(api.searchAppMessages).not.toHaveBeenCalled();
            await vi.advanceTimersByTimeAsync(1);
            await flushPromises();
            expect(api.searchAppMessages).toHaveBeenLastCalledWith({
                path: { appId: APP_ID },
                body: { q: 'ada', field: 'any', limit: 50 },
                signal: expect.any(AbortSignal)
            });
            expect(router.currentRoute.value.fullPath).toBe(path);
            expect(path).not.toContain('?');
            expect(api.listAppMessages.mock.calls.every(call => !JSON.stringify(call).includes('ada'))).toBe(true);
            expect(setItem.mock.calls.every(call => !call.join(' ').includes('ada'))).toBe(true);
            expect(JSON.stringify(router.options.history.state)).not.toContain('ada');
            expect(wrapper!.text()).toContain('1 matching message loaded');
            setItem.mockRestore();
        });

        it('applies a field change immediately and shows the Enhanced Protection note for any and subject only', async () => {
            await mountAt(`/apps/${APP_ID}/messages`);
            const note = async () => (await openHelp(wrapper!, 'message search')).includes("Subjects of Enhanced Protection messages can't be searched");
            const closeHelp = async () => {
                await document.querySelector<HTMLButtonElement>('[role="dialog"] button[type="submit"]')!.click();
                await flushPromises();
            };
            expect(await note()).toBe(true);
            await closeHelp();
            await type('ada');
            await field().setValue('recipient');
            await flushPromises();
            expect(api.searchAppMessages).toHaveBeenLastCalledWith(expect.objectContaining({ body: { q: 'ada', field: 'recipient', limit: 50 } }));
            for (const f of ['id', 'recipient', 'recipient_domain', 'sender_domain']) {
                await field().setValue(f);
                expect(await note()).toBe(false);
                await closeHelp();
            }
            await field().setValue('subject');
            expect(await note()).toBe(true);
            await closeHelp();
            await field().setValue('any');
            expect(await openHelp(wrapper!, 'message search')).toContain('still be found by message or delivery ID');
            await closeHelp();
        });

        it('validates the id field and domain fields without sending a request', async () => {
            await mountAt(`/apps/${APP_ID}/messages`);
            vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
            await field().setValue('id');
            await type('abc');
            await vi.advanceTimersByTimeAsync(400);
            expect(wrapper!.find('.validation').text()).toContain('8 to 32 characters (0-9, a-f)');
            await field().setValue('sender_domain');
            await type('localhost');
            await vi.advanceTimersByTimeAsync(400);
            expect(wrapper!.find('.validation').text()).toContain('full domain name');
            expect(api.searchAppMessages).not.toHaveBeenCalled();
            await type('@Example.COM');
            await vi.advanceTimersByTimeAsync(400);
            await flushPromises();
            expect(api.searchAppMessages).toHaveBeenLastCalledWith(
                expect.objectContaining({ body: { q: 'example.com', field: 'sender_domain', limit: 50 } })
            );
        });

        it('shows a server 400 as an inline hint without echoing the term', async () => {
            api.searchAppMessages.mockRejectedValue(httpError(400));
            await mountAt(`/apps/${APP_ID}/messages`);
            await field().setValue('recipient');
            await type('s3cretterm');
            await wrapper!.find('form').trigger('submit');
            await flushPromises();
            const hint = wrapper!.find('.validation');
            expect(hint.text()).toContain('nothing searchable for Recipient');
            expect(hint.text()).not.toContain('s3cretterm');
            expect(wrapper!.find('.api-error').exists()).toBe(false);
            expect(rows()).toHaveLength(0);
        });

        it('shows a server 503 as unavailable', async () => {
            api.searchAppMessages.mockRejectedValue(httpError(503));
            await mountAt(`/apps/${APP_ID}/messages`);
            await type('ada');
            await wrapper!.find('form').trigger('submit');
            await flushPromises();
            expect(wrapper!.find('.api-error').text()).toContain('Search is temporarily unavailable');
        });

        it('ignores a late response for a superseded query', async () => {
            let resolveFirst: (v: unknown) => void = () => {};
            api.searchAppMessages
                .mockReturnValueOnce(new Promise(resolve => (resolveFirst = resolve)))
                .mockResolvedValueOnce({ data: { messages: [message(9)] } });
            await mountAt(`/apps/${APP_ID}/messages`);
            await type('first');
            await wrapper!.find('form').trigger('submit');
            await flushPromises();
            await type('second');
            await wrapper!.find('form').trigger('submit');
            await flushPromises();
            resolveFirst({ data: { messages: [message(8)] } });
            await flushPromises();
            expect(rows()).toHaveLength(1);
            expect(rows()[0]!.text()).toContain(message(9).id.slice(0, 8));
        });

        it('sends the cursor in the body when loading more search results', async () => {
            api.searchAppMessages
                .mockResolvedValueOnce({ data: { messages: [message(3)], next_cursor: 'c1' } })
                .mockResolvedValueOnce({ data: { messages: [message(2)] } });
            await mountAt(`/apps/${APP_ID}/messages`);
            await type('ada');
            await wrapper!.find('form').trigger('submit');
            await flushPromises();
            await button('Load older messages').trigger('click');
            await flushPromises();
            expect(api.searchAppMessages).toHaveBeenLastCalledWith(
                expect.objectContaining({ body: { q: 'ada', field: 'any', limit: 50, cursor: 'c1' } })
            );
            expect(rows()).toHaveLength(2);
            expect(wrapper!.text()).toContain('end of results');
        });

        it('restores the search from memory after visiting a message, and drops it when the session ends', async () => {
            await mountAt(`/apps/${APP_ID}/messages`);
            await type('ada');
            await wrapper!.find('form').trigger('submit');
            await flushPromises();
            await router.push({ name: 'app-message', params: { appId: APP_ID, messageId: message(2).id } });
            await flushPromises();
            api.searchAppMessages.mockClear();
            router.back();
            await flushPromises();
            expect((input().element as HTMLInputElement).value).toBe('ada');
            expect(api.searchAppMessages).toHaveBeenCalledTimes(1);

            wrapper!.unmount();
            authState.status = 'ended';
            await nextTick();
            authState.status = 'signed-in';
            await mountAt(`/apps/${APP_ID}/messages`);
            expect((input().element as HTMLInputElement).value).toBe('');
        });

        it('navigates to the message on an explicit exact-ID submit', async () => {
            const id = message(2).id;
            await mountAt(`/apps/${APP_ID}/messages`);
            await type(id);
            await wrapper!.find('form').trigger('submit');
            await flushPromises();
            expect(router.currentRoute.value.name).toBe('app-message');
            expect(router.currentRoute.value.params.messageId).toBe(id);
        });

        it('navigates on an exact ID pasted in a provider shape', async () => {
            const id = message(2).id;
            const uuid = `${id.slice(0, 8)}-${id.slice(8, 12)}-${id.slice(12, 16)}-${id.slice(16, 20)}-${id.slice(20)}`;
            for (const pasted of [uuid, `<${id}@example.com>`, `${id}@example.com`]) {
                await mountAt(`/apps/${APP_ID}/messages`);
                api.searchAppMessages.mockClear();
                await type(pasted);
                await wrapper!.find('form').trigger('submit');
                await flushPromises();
                expect(api.searchAppMessages.mock.calls[0]?.[0]).toMatchObject({ body: { q: id } });
                expect(router.currentRoute.value.name).toBe('app-message');
                expect(router.currentRoute.value.params.messageId).toBe(id);
                await router.push(`/apps/${APP_ID}/messages`);
                wrapper!.unmount();
            }
        });

        it('navigates to the only result of an id search (delivery ID match)', async () => {
            api.searchAppMessages.mockResolvedValue({ data: { messages: [message(5)] } });
            await mountAt(`/apps/${APP_ID}/messages`);
            await field().setValue('id');
            await type('d'.repeat(32));
            await wrapper!.find('form').trigger('submit');
            await flushPromises();
            expect(router.currentRoute.value.params.messageId).toBe(message(5).id);
        });

        it('does not navigate for a debounced exact ID', async () => {
            await mountAt(`/apps/${APP_ID}/messages`);
            vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
            await type(message(2).id);
            await vi.advanceTimersByTimeAsync(400);
            await flushPromises();
            expect(api.searchAppMessages).toHaveBeenCalled();
            expect(router.currentRoute.value.name).toBe('app-messages');
        });

        it('clears the search and returns to the plain history', async () => {
            await mountAt(`/apps/${APP_ID}/messages`);
            await type('ada');
            await wrapper!.find('form').trigger('submit');
            await flushPromises();
            await wrapper!.find('button[aria-label="Clear search"]').trigger('click');
            await flushPromises();
            expect(api.listAppMessages).toHaveBeenCalledTimes(2);
            expect((input().element as HTMLInputElement).value).toBe('');
        });

        it('shows the empty search state with the Enhanced Protection note for subject searches', async () => {
            api.searchAppMessages.mockResolvedValue({ data: { messages: [] } });
            await mountAt(`/apps/${APP_ID}/messages`);
            await field().setValue('subject');
            await type('invoice');
            await wrapper!.find('form').trigger('submit');
            await flushPromises();
            expect(wrapper!.text()).toContain('No matches');
            expect(wrapper!.find('.empty .hint').text()).toContain('Excludes Enhanced Protection subjects');
        });
    });

    it('shows an unrecognized status verbatim', async () => {
        api.listAppMessages.mockResolvedValue({ data: { messages: [message(1, { status: 'held_for_review' })] } });
        await mountAt(`/apps/${APP_ID}/messages`);
        expect(rows()[0]!.text()).toContain('held_for_review');
    });

    it('shows an empty state only for a valid empty page', async () => {
        api.listAppMessages.mockResolvedValue({ data: { messages: [] } });
        await mountAt(`/apps/${APP_ID}/messages`);
        expect(wrapper!.text()).toContain('No messages');
    });

    it('treats a malformed page as an error', async () => {
        api.listAppMessages.mockResolvedValue({ data: { messages: [], next_cursor: 42 } });
        await mountAt(`/apps/${APP_ID}/messages`);
        expect(wrapper!.text()).toContain("Thinpost sent back something the console didn't understand");
        expect(wrapper!.text()).not.toContain('No messages');
    });

    it('treats a 404 as a missing app', async () => {
        api.listAppMessages.mockRejectedValue(httpError(404));
        await mountAt(`/apps/${APP_ID}/messages`);
        expect(wrapper!.text()).toContain('The app was not found.');
    });
});
