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
const { describeUsage, COUNT_LIMITS, parsePendingQuota, parseRecipientQuota, validateCountLimit } = await import('@/count-limits');

const APP_ID = '0123456789abcdef0123456789abcdef';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };

let wrapper: VueWrapper | undefined;

async function mountApp() {
    const mounted = await mountAppRoute(`/apps/${APP_ID}`, { 'app-keys': AppKeys });
    wrapper = mounted.wrapper;
}

const dialog = () => wrapper!.find('[role="dialog"]');
const submitButton = () => dialog().find('button[type="submit"]');

interface Case {
    kind: 'recipient' | 'pending';
    title: string;
    noun: string;
    get: ReturnType<typeof vi.fn>;
    set: ReturnType<typeof vi.fn>;
    field: string;
    initial: unknown;
    blocked: unknown;
    /** Response for a saved limit of 250. */
    saved: unknown;
    savedText: string;
    toast: string;
    key: string;
}

const cases: Case[] = [
    {
        kind: 'recipient',
        title: 'Daily recipient limit',
        noun: 'daily recipient limit',
        get: api.getRecipientQuota,
        set: api.setRecipientQuota,
        field: 'Recipients per UTC day',
        initial: { daily_limit: 1000, used: 120, day: '2026-11-19' },
        blocked: { daily_limit: 0, used: 30, day: '2026-11-19' },
        saved: { daily_limit: 250, used: 120, day: '2026-11-19' },
        savedText: '250 recipients per UTC day',
        toast: 'Daily recipient limit for “Billing” set to 250 recipients per UTC day.',
        key: 'daily_limit'
    },
    {
        kind: 'pending',
        title: 'Pending-message limit',
        noun: 'pending-message limit',
        get: api.getPendingQuota,
        set: api.setPendingQuota,
        field: 'Pending messages',
        initial: { limit: 500, pending: 7, truncated: false },
        blocked: { limit: 0, pending: 3, truncated: false },
        saved: { limit: 250, pending: 7, truncated: false },
        savedText: '250 pending messages',
        toast: 'Pending-message limit for “Billing” set to 250 pending messages.',
        key: 'limit'
    }
];

function panel(c: Case) {
    return wrapper!.find(`section.count-limit[data-limit="${c.kind}"]`);
}

function panelButton(c: Case, label: string) {
    const match = panel(c)
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

async function openDialog(c: Case) {
    await panelButton(c, 'Change limit').trigger('click');
    await flushPromises();
    expect(dialog().exists()).toBe(true);
    const firstField = dialog().find('input[inputmode="numeric"]').element;
    if (firstField) await vi.waitFor(() => expect(document.activeElement).toBe(firstField));
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
    api.getRecipientQuota.mockResolvedValue({ data: cases[0]!.initial });
    api.getPendingQuota.mockResolvedValue({ data: cases[1]!.initial });
    api.getAppSendingIp.mockResolvedValue({ data: { sending_ip_id: null, name: null } });
    api.listSendingIps.mockResolvedValue({ data: { ips: [], unpinned_app_count: 0, edge_reported_at: null, edge_reports_sources: false, edge_primary_address: null, unnamed_edge_addresses: [] } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('count limit rules', () => {
    it('mirrors the server range and points to blocking instead of zero', () => {
        expect(validateCountLimit('1000', 1_000_000)).toEqual({ value: 1000, error: null });
        expect(validateCountLimit('1000000', 1_000_000).value).toBe(1_000_000);
        expect(validateCountLimit('1000001', 1_000_000).error).toBe('Enter at most 1,000,000.');
        expect(validateCountLimit('100001', 100_000).error).toBe('Enter at most 100,000.');
        expect(validateCountLimit('', 100).error).toBe('Enter a limit.');
        expect(validateCountLimit('0', 100).error).toContain('choose “Block new messages”');
        for (const raw of ['1.5', '1,000', '1e3', '-5', 'ten']) {
            expect(validateCountLimit(raw, 100).error).toContain('whole number');
        }
    });

    it('accepts only contract-valid responses', () => {
        expect(parseRecipientQuota({ daily_limit: 0, used: 0, day: '2026-11-19' }).limit).toBe(0);
        expect(parsePendingQuota({ limit: 5, pending: 9, truncated: true })).toEqual({ limit: 5, usage: 9, truncated: true, day: null });
        for (const body of [null, '<html>', {}, { daily_limit: 1, used: 0 }, { daily_limit: 1000001, used: 0, day: '2026-11-19' }, { daily_limit: 1, used: -1, day: '2026-11-19' }]) {
            expect(() => parseRecipientQuota(body)).toThrow("something the console didn't understand");
        }
        for (const body of [null, {}, { limit: 1, pending: 0 }, { limit: 100001, pending: 0, truncated: false }, { limit: 1, pending: 0.5, truncated: false }]) {
            expect(() => parsePendingQuota(body)).toThrow("something the console didn't understand");
        }
    });

    it('describes usage and marks a lower bound', () => {
        expect(describeUsage(COUNT_LIMITS.recipient, { limit: 1000, usage: 120, truncated: false, day: '2026-11-19' })).toBe('120 of 1,000 recipients');
        expect(describeUsage(COUNT_LIMITS.pending, { limit: 500, usage: 100000, truncated: true, day: null })).toBe('100,000 or more of 500 messages');
    });
});

describe.each(cases)('$title panel', c => {
    it('loads and explains the current limit', async () => {
        await mountApp();
        expect(c.get).toHaveBeenCalledWith({ path: { appId: APP_ID } });
        const text = panel(c).text();
        expect(text).toContain(c.title);
        expect(text).toContain('Limited');
        expect(text).toContain('As of');
        if (c.kind === 'recipient') {
            expect(text).toContain('1,000 recipients per UTC day');
            expect(text).toContain('120 of 1,000 recipients');
            expect(text).toContain('2026-11-19');
        } else {
            expect(text).toContain('500 pending messages');
            expect(text).toContain('7 of 500 messages');
            expect(text).not.toContain('approximate');
        }
        const help = await openHelp(wrapper!, c.noun);
        expect(help).toContain('A limit of zero rejects every new message');
    });

    it('states plainly when new messages are blocked', async () => {
        c.get.mockResolvedValue({ data: c.blocked });
        await mountApp();
        const text = panel(c).text();
        expect(text).toContain('New messages blocked');
        expect(text).toContain('Every message is rejected until a limit is set');
        expect(text).toContain('New apps start blocked');
        expect(panelButton(c, 'Change limit').attributes('disabled')).toBeUndefined();
    });

    it('shows a loading state without offering changes', async () => {
        c.get.mockReturnValue(new Promise(() => undefined));
        await mountApp();
        expect(panel(c).attributes('aria-busy')).toBe('true');
        expect(panel(c).find('[role="status"]').text()).toBe(`Loading ${c.noun}`);
        expect(panelButton(c, 'Change limit').attributes('disabled')).toBeDefined();
    });

    it('reports load failures and retries', async () => {
        c.get.mockRejectedValueOnce(httpError(503));
        await mountApp();
        const notice = panel(c).find('.api-error');
        expect(notice.text()).toContain(`The ${c.noun} is temporarily unavailable`);
        expect(panelButton(c, 'Change limit').attributes('disabled')).toBeDefined();
        await notice.find('button').trigger('click');
        await flushPromises();
        expect(c.get).toHaveBeenCalledTimes(2);
        expect(panel(c).find('.api-error').exists()).toBe(false);
        expect(panel(c).text()).toContain('Limited');
    });

    it('rejects a response that breaks the contract instead of displaying it', async () => {
        c.get.mockResolvedValue({ data: { nope: true } });
        await mountApp();
        expect(panel(c).find('.api-error').text()).toContain("something the console didn't understand");
        expect(panel(c).text()).not.toContain('Limited');
    });

    it('blocks changes while signed out', async () => {
        await mountApp();
        authState.status = 'ended';
        await flushPromises();
        const change = panelButton(c, 'Change limit');
        expect(change.attributes('disabled')).toBeDefined();
        expect(panel(c).find(`#${CSS.escape(change.attributes('aria-describedby')!)}`).text()).toBe(`Sign in again to change the ${c.noun}.`);
    });

    it('validates accessibly and focuses the field without sending', async () => {
        await mountApp();
        await openDialog(c);
        const input = field(c.field);
        expect(input.attributes('inputmode')).toBe('numeric');
        expect(input.attributes('aria-required')).toBe('true');
        await input.setValue('');
        await submit();
        expect(c.set).not.toHaveBeenCalled();
        expect(input.attributes('aria-invalid')).toBe('true');
        const errorId = input.attributes('aria-describedby')!.split(' ').find(ref => ref.endsWith('-error'))!;
        expect(dialog().find(`#${CSS.escape(errorId)}`).text()).toBe('Enter a limit.');
        expect(document.activeElement).toBe(input.element);
        await input.setValue('0');
        expect(dialog().text()).toContain('choose “Block new messages”');
    });

    it('does not offer saving an unchanged setting', async () => {
        await mountApp();
        await openDialog(c);
        expect(dialog().text()).toContain('This is already the current setting.');
        expect(submitButton().attributes('disabled')).toBeDefined();
        await field(c.field).setValue('250');
        expect(dialog().text()).not.toContain('This is already the current setting.');
        expect(submitButton().attributes('disabled')).toBeUndefined();
    });

    it('saves once and shows the saved setting', async () => {
        let resolve: (value: unknown) => void = () => undefined;
        c.set.mockReturnValue(new Promise(r => (resolve = r)));
        await mountApp();
        await openDialog(c);
        expect(dialog().text()).toContain('Takes effect immediately');
        expect(dialog().text()).toContain('recorded in the audit log');
        await field(c.field).setValue('250');
        await submit();
        expect(c.set).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { [c.key]: 250 } });
        expect(submitButton().text()).toBe('Saving…');
        await submit();
        expect(c.set).toHaveBeenCalledTimes(1);

        resolve({ data: c.saved });
        await flushPromises();
        expect(dialog().exists()).toBe(false);
        expect(panel(c).text()).toContain(c.savedText);
        expect(panel(c).find('.saved[role="status"]').text()).toBe(`Saved: ${c.savedText}.`);
        expect(vf.showToast.mock.calls[0]?.[0].message).toBe(c.toast);
    });

    it('blocks new messages only after acknowledgement', async () => {
        c.set.mockResolvedValue({ data: c.blocked });
        await mountApp();
        await openDialog(c);
        await dialog().find('input[type="radio"][value="disabled"]').setValue(true);
        expect(dialog().find('input[inputmode="numeric"]').exists()).toBe(false);
        expect(dialog().text()).toContain('Every new message will be rejected');
        expect(submitButton().text()).toBe('Block new messages');
        expect(submitButton().classes()).toContain('danger');
        await submit();
        expect(c.set).not.toHaveBeenCalled();
        await dialog().find('input[type="checkbox"]').setValue(true);
        await submit();
        expect(c.set).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { [c.key]: 0 } });
        expect(panel(c).text()).toContain('New messages blocked');
        expect(vf.showToast.mock.calls[0]?.[0].message).toBe('New messages blocked for “Billing”.');
    });

    it('allows messages from the blocked state with an empty field', async () => {
        c.get.mockResolvedValue({ data: c.blocked });
        c.set.mockResolvedValue({ data: c.saved });
        await mountApp();
        await openDialog(c);
        expect((field(c.field).element as HTMLInputElement).value).toBe('');
        await field(c.field).setValue('250');
        await submit();
        expect(c.set).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { [c.key]: 250 } });
        expect(panel(c).text()).toContain('Limited');
    });

    it('keeps the dialog open after a definitive refusal', async () => {
        c.set.mockRejectedValue(httpError(400));
        await mountApp();
        await openDialog(c);
        await field(c.field).setValue('250');
        await submit();
        expect(dialog().find('.api-error').text()).toBe('Thinpost rejected the request. Check what you entered and try again. Nothing was changed.');
        expect((field(c.field).element as HTMLInputElement).value).toBe('250');
        await dialog().findAll('button').find(b => b.text() === 'Cancel')!.trigger('click');
        await flushPromises();
        expect(panel(c).text()).not.toContain('may or may not have gone through');
    });

    it('flags an uncertain outcome until the limit is reloaded', async () => {
        c.set.mockRejectedValue(networkError());
        await mountApp();
        await openDialog(c);
        await field(c.field).setValue('250');
        await submit();
        const message = dialog().find('.api-error').text();
        expect(message).toContain("We can't tell whether the change went through");
        expect(message).toContain('It is safe to save the same value again');
        expect(vf.showToast).not.toHaveBeenCalled();
        await dialog().findAll('button').find(b => b.text() === 'Cancel')!.trigger('click');
        await flushPromises();
        expect(panel(c).text()).toContain('An earlier change may or may not have gone through');

        c.get.mockResolvedValue({ data: c.saved });
        await panelButton(c, 'Reload limit').trigger('click');
        await flushPromises();
        expect(panel(c).text()).not.toContain('may or may not have gone through');
        expect(panel(c).text()).toContain(c.savedText);
    });

    it('treats a malformed success response as an uncertain outcome', async () => {
        c.set.mockResolvedValue({ data: '<html>' });
        await mountApp();
        await openDialog(c);
        await field(c.field).setValue('250');
        await submit();
        expect(dialog().find('.api-error').text()).toContain("We can't tell whether the change went through");
        expect(panel(c).text()).not.toContain('Saved:');
    });
});

describe('pending-message limit panel', () => {
    it('says when the pending count is approximate', async () => {
        api.getPendingQuota.mockResolvedValue({ data: { limit: 500, pending: 100000, truncated: true } });
        await mountApp();
        const text = wrapper!.find('section.count-limit[data-limit="pending"]').text();
        expect(text).toContain('100,000 or more of 500 messages');
        expect(text).toContain('The pending count is approximate');
    });
});
