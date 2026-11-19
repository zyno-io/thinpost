import { flushPromises, type VueWrapper } from '@vue/test-utils';
import type { Router } from 'vue-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, networkError } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppMessages: vi.fn(),
    listAppDomains: vi.fn(),
    sendAppTestMessage: vi.fn(),
    getRuntime: vi.fn(),
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
const { default: AppDomains } = await import('@/screens/app-domains.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const MESSAGE_ID = 'abababababababababababababababab';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const domains = [
    { name: 'mail.example.com', active: true, created_at: '2026-10-01T12:00:00Z', verified_at: '2026-10-01T13:00:00Z' },
    { name: 'pending.example.net', active: false, created_at: '2026-10-01T12:00:00Z' },
    { name: 'send.example.net', active: true, created_at: '2026-10-01T12:00:00Z', verified_at: '2026-10-01T13:00:00Z' }
];
const created = () => ({ data: { id: MESSAGE_ID, accepted_at: '2026-10-02T12:00:00Z' }, response: { status: 201 } });

let wrapper: VueWrapper | undefined;
let router: Router;

async function mountMessages() {
    const mounted = await mountAppRoute(`/apps/${APP_ID}/messages`, {
        'app-messages': AppMessages,
        'app-message': { template: '<div data-stub="message" />' }
    });
    wrapper = mounted.wrapper;
    router = mounted.router;
}

const sendButton = () => wrapper!.findAll('button').find(b => b.text().includes('Send test message') && !b.attributes('type')?.includes('submit'))!;
const dialog = () => document.querySelector('[role="dialog"]') as HTMLElement | null;
const submitButton = () => dialog()!.querySelector('button[type="submit"]') as HTMLButtonElement;

async function setValue(selector: string, value: string) {
    const el = dialog()!.querySelector(selector) as HTMLInputElement | HTMLTextAreaElement;
    el.value = value;
    el.dispatchEvent(new Event('input'));
    await flushPromises();
}

async function openAndFill(to = 'one@example.org\ntwo@example.org') {
    await sendButton().trigger('click');
    await flushPromises();
    await setValue('textarea.recipients', to);
}

async function submit() {
    submitButton().click();
    await flushPromises();
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    clearSavedSearches();
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.listAppMessages.mockResolvedValue({ data: { messages: [] } });
    api.listAppDomains.mockResolvedValue({ data: { domains } });
    api.getRuntime.mockResolvedValue({ data: { features: { submission: true, tracking: true } } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
    document.body.innerHTML = '';
});

describe('send test message from the messages tab', () => {
    it('is disabled with a reason when the app is paused', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        await mountMessages();
        expect(sendButton().attributes('disabled')).toBeDefined();
        expect(wrapper!.text()).toContain('Resume the app to send');
    });

    it('is disabled when the server reports no sending feature', async () => {
        api.getRuntime.mockResolvedValue({ data: { features: { submission: false, tracking: false } } });
        await mountMessages();
        expect(sendButton().attributes('disabled')).toBeDefined();
        expect(wrapper!.text()).toContain('Sending is not enabled');
    });

    it('is disabled when not signed in', async () => {
        authState.status = 'signed-out';
        await mountMessages();
        expect(sendButton().attributes('disabled')).toBeDefined();
    });

    it('stays available when the runtime report cannot be read', async () => {
        api.getRuntime.mockRejectedValue(httpError(503));
        await mountMessages();
        expect(sendButton().attributes('disabled')).toBeUndefined();
    });

    it('offers only active domains and shows the notes', async () => {
        await mountMessages();
        await sendButton().trigger('click');
        await flushPromises();
        const options = [...dialog()!.querySelectorAll('select option')].map(o => o.textContent);
        expect(options).toEqual(['mail.example.com', 'send.example.net']);
        expect(dialog()!.textContent).toContain('webhooks receive events');
        expect(dialog()!.textContent).toContain('that address is suppressed for this app');
        expect(dialog()!.textContent).toContain('daily recipient limit');
    });

    it('explains when no domain is verified', async () => {
        api.listAppDomains.mockResolvedValue({ data: { domains: [domains[1]] } });
        await mountMessages();
        await sendButton().trigger('click');
        await flushPromises();
        expect(dialog()!.querySelector('[data-testid="no-domains"]')).not.toBeNull();
        expect(submitButton().disabled).toBe(true);
    });

    it('sends the constrained request and opens the message', async () => {
        api.sendAppTestMessage.mockResolvedValue(created());
        await mountMessages();
        await openAndFill();
        await setValue('input[type="text"]', 'hello');
        const name = dialog()!.querySelectorAll('input[type="text"]')[1] as HTMLInputElement;
        name.value = 'Team, Example';
        name.dispatchEvent(new Event('input'));
        const boxes = dialog()!.querySelectorAll<HTMLInputElement>('input[type="checkbox"]');
        boxes[boxes.length - 1]!.click();
        await flushPromises();
        await submit();

        expect(api.sendAppTestMessage).toHaveBeenCalledTimes(1);
        const arg = api.sendAppTestMessage.mock.calls[0]![0];
        expect(arg.path).toEqual({ appId: APP_ID });
        expect(arg.body).toMatchObject({
            from: '"Team, Example" <hello@mail.example.com>',
            to: ['one@example.org', 'two@example.org'],
            subject: 'Test message from Thinpost',
            enhanced_protection: true
        });
        expect(Object.keys(arg.body).sort()).toEqual(['enhanced_protection', 'from', 'subject', 'text', 'to']);
        expect(vf.showToast).toHaveBeenCalled();
        expect(router.currentRoute.value.name).toBe('app-message');
        expect(router.currentRoute.value.params.messageId).toBe(MESSAGE_ID);
        expect(dialog()).toBeNull();
    });

    it('does not send more than five recipients', async () => {
        await mountMessages();
        await openAndFill(Array.from({ length: 6 }, (_, i) => `u${i}@example.org`).join('\n'));
        expect(submitButton().disabled).toBe(true);
    });

    it.each([
        ['submission_rate_exceeded', 429, 'Sending limit'],
        ['recipient_quota_exceeded', 429, 'daily recipient limit'],
        ['pending_limit_exceeded', 429, 'pending-message limit'],
        ['message_rejected', 422, 'verified sending domain'],
        ['app_paused', 409, 'paused']
    ])('shows copy for %s and keeps the form open', async (code, status, copy) => {
        api.sendAppTestMessage.mockRejectedValue(httpError(status, { error: code }));
        await mountMessages();
        await openAndFill();
        await submit();
        expect(dialog()).not.toBeNull();
        expect(dialog()!.textContent).toContain(copy);
        expect(dialog()!.textContent).toContain('Nothing was sent');
        expect(submitButton().disabled).toBe(false);
    });

    it.each([
        ['no response', () => networkError()],
        ['a 503', () => httpError(503, { error: 'Service Unavailable' })],
        ['a malformed success', null]
    ])('blocks resending after %s until the list reloads', async (_name, failure) => {
        if (failure) api.sendAppTestMessage.mockRejectedValue(failure());
        else api.sendAppTestMessage.mockResolvedValue({ data: { id: 'nope' }, response: { status: 201 } });
        api.listAppMessages.mockResolvedValueOnce({ data: { messages: [] } }).mockRejectedValueOnce(httpError(503));
        await mountMessages();
        await openAndFill();
        await submit();

        expect(api.sendAppTestMessage).toHaveBeenCalledTimes(1);
        expect(dialog()).toBeNull();
        expect(wrapper!.text()).toContain("can't tell whether the message was sent");
        // The reload failed, so the block holds.
        expect(sendButton().attributes('disabled')).toBeDefined();
        expect(wrapper!.text()).toContain('Wait for the message list to reload');
        expect(api.sendAppTestMessage).toHaveBeenCalledTimes(1);
    });

    it('allows another send once the list reloaded after an unknown outcome', async () => {
        api.sendAppTestMessage.mockRejectedValue(networkError());
        await mountMessages();
        await openAndFill();
        await submit();
        expect(api.listAppMessages).toHaveBeenCalledTimes(2);
        expect(sendButton().attributes('disabled')).toBeUndefined();
        expect(api.sendAppTestMessage).toHaveBeenCalledTimes(1);
    });
});

describe('send test from the sending domains tab', () => {
    it('offers Send test on verified domains only and preselects the domain', async () => {
        api.sendAppTestMessage.mockResolvedValue(created());
        const mounted = await mountAppRoute(`/apps/${APP_ID}/domains`, {
            'app-domains': AppDomains,
            'app-message': { template: '<div data-stub="message" />' }
        });
        wrapper = mounted.wrapper;
        const buttons = wrapper.findAll('button').filter(b => b.text().includes('Send test'));
        expect(buttons).toHaveLength(2);
        await buttons[1]!.trigger('click');
        await flushPromises();
        expect((dialog()!.querySelector('select') as HTMLSelectElement).value).toBe('send.example.net');
    });
});
