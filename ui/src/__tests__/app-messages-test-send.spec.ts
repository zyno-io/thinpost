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
const key = { selector: 'thinpost1', dns_name: 'thinpost1._domainkey.example.com', dns_value: 'v=DKIM1; k=rsa; p=AAAA', activated_at: '2026-10-01T13:00:00Z' };
const domains = [
    { name: 'mail.example.com', active: true, created_at: '2026-10-01T12:00:00Z', verified_at: '2026-10-01T13:00:00Z', active_key: key },
    { name: 'pending.example.net', active: false, created_at: '2026-10-01T12:00:00Z' },
    { name: 'send.example.net', active: true, created_at: '2026-10-01T12:00:00Z', verified_at: '2026-10-01T13:00:00Z', active_key: key }
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

    it('offers only active domains with a DKIM key and shows the notes', async () => {
        const noKey = { name: 'nokey.example.org', active: true, created_at: '2026-10-01T12:00:00Z', verified_at: '2026-10-01T13:00:00Z' };
        api.listAppDomains.mockResolvedValue({ data: { domains: [...domains, noKey] } });
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
        const boxes = [...dialog()!.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')];
        boxes.find(box => box.parentElement?.textContent?.includes('Enhanced Protection'))!.click();
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

function checkbox(testid: string) {
    return dialog()!.querySelector<HTMLInputElement>(`[data-testid="${testid}"] input[type="checkbox"]`);
}

async function openForm() {
    await sendButton().trigger('click');
    await flushPromises();
    await setValue('textarea.recipients', 'one@example.org\ntwo@example.org');
}

describe('operator test-send options', () => {
    it('offers the limits option, off by default, and sends no flag unless it is on', async () => {
        api.sendAppTestMessage.mockResolvedValue(created());
        await mountMessages();
        await openForm();
        expect(checkbox('bypass-limits')).not.toBeNull();
        expect(checkbox('bypass-limits')!.checked).toBe(false);
        expect(dialog()!.textContent).toContain('Not checked or counted against the sending rate, daily recipient quota or pending limit');
        expect(dialog()!.textContent).toContain('Verified domain, DKIM, suppressions and pause still apply');
        await submit();
        const body = api.sendAppTestMessage.mock.calls[0]![0].body;
        expect(body).not.toHaveProperty('bypass_limits');
        expect(body).not.toHaveProperty('bypass_recipient_policy');
    });

    it('sends bypass_limits when ticked, without a confirmation', async () => {
        api.sendAppTestMessage.mockResolvedValue(created());
        await mountMessages();
        await openForm();
        checkbox('bypass-limits')!.click();
        await flushPromises();
        await submit();
        expect(vf.showConfirm).not.toHaveBeenCalled();
        const body = api.sendAppTestMessage.mock.calls[0]![0].body;
        expect(body.bypass_limits).toBe(true);
        expect(body).not.toHaveProperty('bypass_recipient_policy');
    });

    it.each([
        ['an unrestricted deployment', { submission: true, tracking: true, recipient_policy: 'unrestricted' }],
        ['a server that does not report the policy', { submission: true, tracking: true }]
    ])('hides the allowlist option for %s', async (_name, features) => {
        api.getRuntime.mockResolvedValue({ data: { features } });
        await mountMessages();
        await openForm();
        expect(checkbox('bypass-recipient-policy')).toBeNull();
        expect(dialog()!.textContent).not.toContain('recipient allowlist');
    });

    describe('with a recipient allowlist', () => {
        beforeEach(() => {
            api.getRuntime.mockResolvedValue({ data: { features: { submission: true, tracking: true, recipient_policy: 'allowlist' } } });
        });

        it('shows the option off by default with its warning', async () => {
            await mountMessages();
            await openForm();
            expect(checkbox('bypass-recipient-policy')).not.toBeNull();
            expect(checkbox('bypass-recipient-policy')!.checked).toBe(false);
            expect(dialog()!.textContent).toContain("Can deliver to people outside this deployment's recipient allowlist.");
        });

        it('asks for a second confirmation naming the recipient count and sends only when confirmed', async () => {
            api.sendAppTestMessage.mockResolvedValue(created());
            vf.showConfirm.mockResolvedValue(true);
            await mountMessages();
            await openForm();
            checkbox('bypass-recipient-policy')!.click();
            await flushPromises();
            await submit();
            expect(vf.showConfirm).toHaveBeenCalledTimes(1);
            expect(vf.showConfirm.mock.calls[0]![1]).toContain('2 recipients');
            const body = api.sendAppTestMessage.mock.calls[0]![0].body;
            expect(body.bypass_recipient_policy).toBe(true);
            expect(body).not.toHaveProperty('bypass_limits');
        });

        it('does not send when the confirmation is declined, and keeps the form open', async () => {
            vf.showConfirm.mockResolvedValue(false);
            await mountMessages();
            await openForm();
            checkbox('bypass-recipient-policy')!.click();
            await flushPromises();
            await submit();
            expect(vf.showConfirm).toHaveBeenCalledTimes(1);
            expect(api.sendAppTestMessage).not.toHaveBeenCalled();
            expect(dialog()).not.toBeNull();
            expect(submitButton().disabled).toBe(false);
        });

        it('sends both flags when both are ticked', async () => {
            api.sendAppTestMessage.mockResolvedValue(created());
            vf.showConfirm.mockResolvedValue(true);
            await mountMessages();
            await openForm();
            checkbox('bypass-limits')!.click();
            checkbox('bypass-recipient-policy')!.click();
            await flushPromises();
            await submit();
            const body = api.sendAppTestMessage.mock.calls[0]![0].body;
            expect(body.bypass_limits).toBe(true);
            expect(body.bypass_recipient_policy).toBe(true);
        });
    });
});

describe('console test tags in the message list', () => {
    const row = (id: string, operatorTest?: { bypass_limits: boolean; bypass_recipient_policy: boolean }) => ({
        id,
        state: 'prepared',
        status: 'delivered',
        enhanced_protection: false,
        subject: 'Hello',
        subject_masked: false,
        accepted_at: '2026-10-02T12:00:00Z',
        delivery_count: 1,
        accepted_count: 1,
        failed_count: 0,
        uncertain_count: 0,
        suppressed_count: 0,
        ...(operatorTest ? { operator_test: operatorTest } : {})
    });

    it('tags console tests and what they skipped, and nothing else', async () => {
        api.listAppMessages.mockResolvedValue({
            data: {
                messages: [
                    row('1'.repeat(32)),
                    row('2'.repeat(32), { bypass_limits: false, bypass_recipient_policy: false }),
                    row('3'.repeat(32), { bypass_limits: true, bypass_recipient_policy: false }),
                    row('4'.repeat(32), { bypass_limits: true, bypass_recipient_policy: true })
                ]
            }
        });
        await mountMessages();
        const rows = wrapper!.findAll('tbody tr');
        expect(rows).toHaveLength(4);
        const tags = (i: number) => ({
            test: rows[i]!.find('[data-testid="console-test-tag"]').exists(),
            limits: rows[i]!.find('[data-testid="limits-skipped-tag"]').exists(),
            allowlist: rows[i]!.find('[data-testid="allowlist-skipped-tag"]').exists()
        });
        expect(tags(0)).toEqual({ test: false, limits: false, allowlist: false });
        expect(tags(1)).toEqual({ test: true, limits: false, allowlist: false });
        expect(tags(2)).toEqual({ test: true, limits: true, allowlist: false });
        expect(tags(3)).toEqual({ test: true, limits: true, allowlist: true });
    });
});

describe('send test from the sending domains tab', () => {
    it('offers Send test on verified domains with a DKIM key only and preselects the domain', async () => {
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
