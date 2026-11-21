import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, networkError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppKeys: vi.fn(),
    getSubmissionRate: vi.fn(),
    getRecipientQuota: vi.fn(),
    setRecipientQuota: vi.fn(),
    getPendingQuota: vi.fn(),
    setPendingQuota: vi.fn(),
    getAppSendingIp: vi.fn(),
    listSendingIps: vi.fn(),
    setSubmissionRate: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({
    showConfirm: vi.fn(),
    showConfirmDestroy: vi.fn(),
    showToast: vi.fn()
}));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { default: AppKeys } = await import('@/screens/app-keys.vue');
const { describeRefill, describeSustained, parseSubmissionRate, validateBurst, validatePerMinute } = await import('@/submission-rate');

const APP_ID = '0123456789abcdef0123456789abcdef';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };

let wrapper: VueWrapper | undefined;

async function mountApp() {
    const mounted = await mountAppRoute(`/apps/${APP_ID}`, { 'app-keys': AppKeys });
    wrapper = mounted.wrapper;
}

const panel = () => wrapper!.find('section.submission-rate');
const dialog = () => wrapper!.find('[role="dialog"]');
const submitButton = () => dialog().find('button[type="submit"]');

function panelButton(label: string) {
    const match = panel()
        .findAll('button')
        .find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}

function field(label: string) {
    const labelEl = dialog()
        .findAll('label')
        .find(l => l.text().startsWith(label));
    if (!labelEl) throw new Error(`label "${label}" not found`);
    return dialog().find(`#${CSS.escape(labelEl.attributes('for')!)}`);
}

async function openDialog() {
    await panelButton('Change limit').trigger('click');
    await flushPromises();
    expect(dialog().exists()).toBe(true);
    // v-autofocus (vue-foundation) focuses the first field from a 10 ms timer scheduled at mount. Wait for it
    // to land so it cannot fire mid-test (under load) and steal focus that a later step moved deliberately.
    const firstField = field('Messages per minute').element;
    await vi.waitFor(() => expect(document.activeElement).toBe(firstField));
}

async function submit() {
    await dialog().find('form').trigger('submit');
    await flushPromises();
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.listAppKeys.mockResolvedValue({ data: { keys: [] } });
    api.getSubmissionRate.mockResolvedValue({ data: { per_minute: 600, burst: 100 } });
    api.getRecipientQuota.mockResolvedValue({ data: { daily_limit: 1000, used: 120, day: '2026-11-19' } });
    api.getPendingQuota.mockResolvedValue({ data: { limit: 500, pending: 7, truncated: false } });
    api.getAppSendingIp.mockResolvedValue({ data: { sending_ip_id: null, address: null, helo_name: null } });
    api.listSendingIps.mockResolvedValue({ data: { ips: [], unpinned_app_count: 0, edge_reported_at: null, edge_reports_identity: false, edge_primary_address: null } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('submission rate rules', () => {
    it('mirrors the server range for each field and points to disabling instead of zero', () => {
        expect(validatePerMinute('600')).toEqual({ value: 600, error: null });
        expect(validatePerMinute('60000').value).toBe(60000);
        expect(validatePerMinute('60001').error).toBe('Enter at most 60,000.');
        expect(validateBurst('10000').value).toBe(10000);
        expect(validateBurst('10001').error).toBe('Enter at most 10,000.');
        expect(validatePerMinute('').error).toBe('Enter a number of messages per minute.');
        expect(validateBurst('  ').error).toBe('Enter a burst size.');
        expect(validateBurst('0').error).toContain('To reject all new messages, choose “Block new messages”');
        for (const raw of ['1.5', '1,000', '1e3', '-5', 'ten']) {
            expect(validatePerMinute(raw).error).toContain('whole number');
        }
    });

    it('accepts only contract-valid responses', () => {
        expect(parseSubmissionRate({ per_minute: 0, burst: 0 })).toEqual({ per_minute: 0, burst: 0 });
        expect(parseSubmissionRate({ per_minute: 60000, burst: 10000 })).toEqual({ per_minute: 60000, burst: 10000 });
        for (const body of [null, '<html>', {}, { per_minute: 0, burst: 5 }, { per_minute: 5, burst: 0 }, { per_minute: 1.5, burst: 1 }, { per_minute: 60001, burst: 1 }, { per_minute: 1, burst: 10001 }]) {
            expect(() => parseSubmissionRate(body)).toThrow("something the console didn't understand");
        }
    });

    it('describes the sustained rate and refill time', () => {
        expect(describeSustained(600)).toBe('600 messages per minute (10 per second)');
        expect(describeSustained(90)).toBe('90 messages per minute (1.5 per second)');
        expect(describeSustained(30)).toBe('30 messages per minute (one every 2 seconds)');
        expect(describeSustained(1)).toBe('1 message per minute (one every 60 seconds)');
        expect(describeRefill({ per_minute: 600, burst: 100 })).toBe('10 seconds');
        expect(describeRefill({ per_minute: 1, burst: 10000 })).toBe('7 days');
    });
});

describe('submission rate panel', () => {
    it('loads and explains the current limit', async () => {
        await mountApp();
        expect(api.getSubmissionRate).toHaveBeenCalledWith({ path: { appId: APP_ID } });
        const text = panel().text();
        expect(text).toContain('Limited');
        expect(text).toContain('600 messages per minute (10 per second)');
        expect(text).toContain('100 messages');
        expect(text).toContain('Time to refill');
        expect(text).toContain('10 seconds');
        expect(text).toContain('As of');
        const help = await openHelp(wrapper!, 'sending limit');
        expect(help).toContain('This limits how fast this app\'s API keys can send messages to Thinpost');
        expect(help).toContain('Each message uses one token');
        expect(help).toContain('A Resend batch needs tokens for all of its messages at once');
        expect(help).toContain('Retries, including retries with the same idempotency key, and requests that fail afterwards still use tokens');
    });

    it('states plainly when new messages are blocked', async () => {
        api.getSubmissionRate.mockResolvedValue({ data: { per_minute: 0, burst: 0 } });
        await mountApp();
        const text = panel().text();
        expect(text).toContain('New messages blocked');
        expect(text).toContain('Every message is rejected until a limit is set');
        expect(text).toContain('New apps start blocked');
        expect(text).not.toContain('Time to refill');
        expect(panelButton('Change limit').attributes('disabled')).toBeUndefined();
    });

    it('shows a loading state without offering changes', async () => {
        api.getSubmissionRate.mockReturnValue(new Promise(() => undefined));
        await mountApp();
        expect(panel().attributes('aria-busy')).toBe('true');
        expect(panel().find('[role="status"]').text()).toBe('Loading sending limit');
        expect(panelButton('Change limit').attributes('disabled')).toBeDefined();
    });

    it('reports load failures and retries', async () => {
        api.getSubmissionRate.mockRejectedValueOnce(httpError(503));
        await mountApp();
        const notice = panel().find('.api-error');
        expect(notice.text()).toContain('The sending limit is temporarily unavailable');
        expect(panelButton('Change limit').attributes('disabled')).toBeDefined();
        expect(panel().text()).not.toContain('Limited');

        await notice.find('button').trigger('click');
        await flushPromises();
        expect(api.getSubmissionRate).toHaveBeenCalledTimes(2);
        expect(panel().find('.api-error').exists()).toBe(false);
        expect(panel().text()).toContain('600 messages per minute');
    });

    it('rejects a response that breaks the contract instead of displaying it', async () => {
        api.getSubmissionRate.mockResolvedValue({ data: { per_minute: 0, burst: 5 } });
        await mountApp();
        expect(panel().find('.api-error').text()).toContain("something the console didn't understand");
        expect(panel().text()).not.toContain('New messages blocked');
    });

    it('blocks changes while signed out', async () => {
        await mountApp();
        authState.status = 'ended';
        await flushPromises();
        const change = panelButton('Change limit');
        expect(change.attributes('disabled')).toBeDefined();
        const hint = panel().find(`#${CSS.escape(change.attributes('aria-describedby')!.split(' ').find(ref => ref.endsWith('-blocker'))!)}`);
        expect(hint.text()).toBe('Sign in again to change the sending limit.');
    });
});

describe('changing the submission rate', () => {
    it('validates accessibly and focuses the first invalid field without sending', async () => {
        await mountApp();
        await openDialog();
        const perMinute = field('Messages per minute');
        const burst = field('Burst (messages)');
        expect((perMinute.element as HTMLInputElement).value).toBe('600');
        expect((burst.element as HTMLInputElement).value).toBe('100');
        expect(perMinute.attributes('inputmode')).toBe('numeric');
        expect(perMinute.attributes('aria-required')).toBe('true');

        await perMinute.setValue('');
        await burst.setValue('20000');
        await submit();

        expect(api.setSubmissionRate).not.toHaveBeenCalled();
        expect(perMinute.attributes('aria-invalid')).toBe('true');
        expect(burst.attributes('aria-invalid')).toBe('true');
        const errorId = perMinute.attributes('aria-describedby')!.split(' ').find(ref => ref.endsWith('-error'))!;
        expect(dialog().find(`#${CSS.escape(errorId)}`).text()).toBe('Enter a number of messages per minute.');
        expect(burst.attributes('aria-describedby')).toContain('-burst-hint');
        expect(dialog().text()).toContain('Enter at most 10,000.');
        expect(document.activeElement).toBe(perMinute.element);

        await perMinute.setValue('1200');
        await submit();
        expect(api.setSubmissionRate).not.toHaveBeenCalled();
        expect(document.activeElement).toBe(burst.element);
        expect(perMinute.attributes('aria-invalid')).toBe('false');
    });

    it('does not offer saving an unchanged setting', async () => {
        await mountApp();
        await openDialog();
        expect(dialog().text()).toContain('This is already the current setting.');
        expect(submitButton().attributes('disabled')).toBeDefined();
        await field('Burst (messages)').setValue('150');
        expect(dialog().text()).not.toContain('This is already the current setting.');
        expect(submitButton().attributes('disabled')).toBeUndefined();
    });

    it('previews the effect, saves once and shows the saved setting', async () => {
        let resolve: (value: unknown) => void = () => undefined;
        api.setSubmissionRate.mockReturnValue(new Promise(r => (resolve = r)));
        await mountApp();
        await openDialog();
        expect(dialog().text()).toContain('Takes effect immediately');
        expect(dialog().text()).toContain('The change is recorded in the audit log');

        await field('Messages per minute').setValue('30');
        await field('Burst (messages)').setValue('20');
        const text = dialog().text();
        expect(text).toContain('Up to 20 messages at once, then 30 messages per minute (one every 2 seconds)');
        expect(text).toContain('An empty bucket takes 40 seconds to refill');
        expect(text).toContain('Resend batches of more than 20 messages will always be rejected');

        await submit();
        expect(api.setSubmissionRate).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { per_minute: 30, burst: 20 } });
        expect(submitButton().text()).toBe('Saving…');
        expect(submitButton().attributes('disabled')).toBeDefined();
        expect(dialog().findAll('button').find(b => b.text() === 'Cancel')!.attributes('disabled')).toBeDefined();
        await submit();
        expect(api.setSubmissionRate).toHaveBeenCalledTimes(1);

        resolve({ data: { per_minute: 30, burst: 20 } });
        await flushPromises();
        expect(dialog().exists()).toBe(false);
        expect(panel().text()).toContain('30 messages per minute (one every 2 seconds)');
        expect(panel().find('.saved[role="status"]').text()).toBe('Saved: 30 per minute with a burst of 20.');
        expect(vf.showToast.mock.calls[0]?.[0].message).toBe('Sending limit for “Billing” set to 30 per minute with a burst of 20.');
    });

    it('blocks new messages only after acknowledgement', async () => {
        api.setSubmissionRate.mockResolvedValue({ data: { per_minute: 0, burst: 0 } });
        await mountApp();
        await openDialog();
        await dialog().find('input[type="radio"][value="disabled"]').setValue(true);

        expect(dialog().find('input[inputmode="numeric"]').exists()).toBe(false);
        expect(dialog().text()).toContain('Every new message will be rejected');
        expect(submitButton().text()).toBe('Block new messages');
        expect(submitButton().classes()).toContain('danger');
        expect(submitButton().attributes('disabled')).toBeDefined();
        await submit();
        expect(api.setSubmissionRate).not.toHaveBeenCalled();

        await dialog().find('input[type="checkbox"]').setValue(true);
        await submit();
        expect(api.setSubmissionRate).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { per_minute: 0, burst: 0 } });
        expect(panel().text()).toContain('New messages blocked');
        expect(vf.showToast.mock.calls[0]?.[0].message).toBe('New messages blocked for “Billing”.');
    });

    it('allows messages from the disabled state with empty fields', async () => {
        api.getSubmissionRate.mockResolvedValue({ data: { per_minute: 0, burst: 0 } });
        api.setSubmissionRate.mockResolvedValue({ data: { per_minute: 600, burst: 100 } });
        await mountApp();
        await openDialog();
        expect(dialog().text()).toContain('Allowing messages again starts with a full burst');
        expect((field('Messages per minute').element as HTMLInputElement).value).toBe('');
        // v-autofocus focuses after a short delay.
        await new Promise(resolve => setTimeout(resolve, 20));
        expect(document.activeElement).toBe(field('Messages per minute').element);

        await field('Messages per minute').setValue('600');
        await field('Burst (messages)').setValue('100');
        expect(dialog().text()).not.toContain('Resend batches of more than');
        await submit();
        expect(api.setSubmissionRate).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { per_minute: 600, burst: 100 } });
        expect(panel().text()).toContain('Limited');
    });

    it('keeps the dialog open after a definitive refusal', async () => {
        api.setSubmissionRate.mockRejectedValue(httpError(400));
        await mountApp();
        await openDialog();
        await field('Burst (messages)').setValue('150');
        await submit();

        expect(dialog().find('.api-error').text()).toBe('Thinpost rejected the request. Check what you entered and try again. Nothing was changed.');
        expect((field('Burst (messages)').element as HTMLInputElement).value).toBe('150');
        await dialog().findAll('button').find(b => b.text() === 'Cancel')!.trigger('click');
        await flushPromises();
        expect(panel().text()).not.toContain('may or may not have gone through');
        expect(panel().text()).toContain('100 messages');
    });

    it('flags an uncertain outcome until the limit is reloaded', async () => {
        api.setSubmissionRate.mockRejectedValue(networkError());
        await mountApp();
        await openDialog();
        await field('Burst (messages)').setValue('150');
        await submit();

        const message = dialog().find('.api-error').text();
        expect(message).toContain("Couldn't reach Thinpost");
        expect(message).toContain("We can't tell whether the change went through");
        expect(message).toContain('It is safe to save the same values again');
        expect(vf.showToast).not.toHaveBeenCalled();

        await dialog().findAll('button').find(b => b.text() === 'Cancel')!.trigger('click');
        await flushPromises();
        expect(panel().text()).toContain('An earlier change may or may not have gone through');

        api.getSubmissionRate.mockResolvedValue({ data: { per_minute: 600, burst: 150 } });
        await panelButton('Reload limit').trigger('click');
        await flushPromises();
        expect(panel().text()).not.toContain('may or may not have gone through');
        expect(panel().text()).toContain('150 messages');
        expect(api.setSubmissionRate).toHaveBeenCalledTimes(1);
    });

    it('treats a malformed success response as an uncertain outcome', async () => {
        api.setSubmissionRate.mockResolvedValue({ data: '<html>' });
        await mountApp();
        await openDialog();
        await field('Burst (messages)').setValue('150');
        await submit();
        expect(dialog().find('.api-error').text()).toContain("something the console didn't understand");
        expect(dialog().find('.api-error').text()).toContain("We can't tell whether the change went through");
        expect(panel().text()).not.toContain('Saved:');
    });
});
