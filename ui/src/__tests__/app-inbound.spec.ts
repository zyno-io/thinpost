import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, networkError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppWebhooks: vi.fn(),
    listInboundDomains: vi.fn(),
    listInboundMessages: vi.fn(),
    requestInboundDomain: vi.fn(),
    verifyInboundDomain: vi.fn(),
    checkInboundDomainMx: vi.fn(),
    disableInboundDomain: vi.fn(),
    enableInboundDomain: vi.fn(),
    deleteInboundDomain: vi.fn(),
    getInboundLimits: vi.fn(),
    setInboundLimits: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({
    showConfirm: vi.fn(),
    showToast: vi.fn()
}));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { default: AppInbound } = await import('@/screens/app-inbound.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const MX_HOST = 'smtp.mx.example.net';
const pending = { name: 'in.example.com', state: 'pending', created_at: '2026-10-02T12:00:00Z' };
const active = { ...pending, state: 'active', verified_at: '2026-10-03T12:00:00Z' };
const disabled = { ...active, state: 'disabled' };
const CHECKED = '2026-10-04T09:30:00Z';
const INBOUND_HOOK = { id: '11111111111111111111111111111111', format: 'thinpost_inbound', endpoint: 'https://hooks.example.com/in', description: '', active: true, created_at: '2026-10-02T12:00:00Z' };
const OUTBOUND_HOOK = { ...INBOUND_HOOK, id: '22222222222222222222222222222222', format: 'thinpost' };
const LIMITS = { daily_limit: 2000, pending_limit: 500 };
const CHALLENGE = { domain: pending, dns_name: '_thinpost.in.example.com', dns_value: 'thinpost-inbound-verification=abc123', mx_host: MX_HOST };

let wrapper: VueWrapper | undefined;

async function mountPage() {
    const mounted = await mountAppRoute(`/apps/${APP_ID}/inbound`, { 'app-inbound': AppInbound });
    wrapper = mounted.wrapper;
    return wrapper;
}

function button(label: string) {
    const match = wrapper!.findAll('button').find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}
const hasButton = (label: string) => wrapper!.findAll('button').some(b => b.text().includes(label));

/** A listing; pass null for a deployment with no receiving mail server configured. */
function list(domains: unknown[], mxHost: string | null = MX_HOST) {
    return { data: { domains, ...(mxHost ? { mx_host: mxHost } : {}) } };
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    localStorage.clear();
    sessionStorage.clear();
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.getInboundLimits.mockResolvedValue({ data: LIMITS });
    api.listAppWebhooks.mockResolvedValue({ data: { webhooks: [INBOUND_HOOK] } });
    api.listInboundMessages.mockResolvedValue({ data: { messages: [] } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('inbound screen', () => {
    it('no longer says receiving is unavailable', async () => {
        api.listInboundDomains.mockResolvedValue(list([active]));
        await mountPage();
        expect(wrapper!.find('[data-testid="inbound-unavailable"]').exists()).toBe(false);
        expect(wrapper!.text()).not.toContain("isn't available yet");
        expect(wrapper!.text()).not.toContain('returned to the sender');
        expect(wrapper!.find('[data-testid="no-inbound-webhook"]').exists()).toBe(false);
    });

    it('warns when the app has no inbound webhook and links to Webhooks', async () => {
        api.listInboundDomains.mockResolvedValue(list([active]));
        api.listAppWebhooks.mockResolvedValue({ data: { webhooks: [OUTBOUND_HOOK] } });
        await mountPage();
        const warning = wrapper!.get('[data-testid="no-inbound-webhook"]');
        expect(warning.text()).toContain('No inbound webhook');
        expect(warning.text()).toContain('Mail to your domains is deferred until you add one');
        // The query asks the Webhooks page to open its add dialog with the inbound format; it carries no data.
        expect(warning.get('a').attributes('href')).toBe(`/apps/${APP_ID}/webhooks?add=thinpost_inbound`);
    });

    it('warns when the only inbound webhook is disabled', async () => {
        api.listInboundDomains.mockResolvedValue(list([active]));
        api.listAppWebhooks.mockResolvedValue({ data: { webhooks: [{ ...INBOUND_HOOK, active: false }] } });
        await mountPage();
        expect(wrapper!.find('[data-testid="no-inbound-webhook"]').exists()).toBe(true);
    });

    it('leaves the warning out when the webhook list cannot be read', async () => {
        api.listInboundDomains.mockResolvedValue(list([active]));
        api.listAppWebhooks.mockRejectedValue(httpError(503));
        await mountPage();
        expect(wrapper!.find('[data-testid="no-inbound-webhook"]').exists()).toBe(false);
        expect(wrapper!.text()).toContain('in.example.com');
    });

    it('shows the empty state with no domains', async () => {
        api.listInboundDomains.mockResolvedValue(list([]));
        await mountPage();
        expect(wrapper!.text()).toContain('No receiving domains');
        expect(wrapper!.find('table').exists()).toBe(false);
        expect(hasButton('Add domain')).toBe(true);
    });

    it('lists a pending domain with Verify and new records, and no enable or disable', async () => {
        api.listInboundDomains.mockResolvedValue(list([pending]));
        await mountPage();
        const text = wrapper!.text();
        expect(text).toContain('in.example.com');
        expect(text).toContain('Not verified');
        expect(text).toContain('Not checked');
        expect(hasButton('Verify')).toBe(true);
        expect(hasButton('New DNS records')).toBe(true);
        expect(hasButton('Disable')).toBe(false);
        expect(hasButton('Enable')).toBe(false);
    });

    it('shows the MX host for a verified domain only, and only when one is configured', async () => {
        api.listInboundDomains.mockResolvedValue(list([pending, { ...active, name: 'ok.example.com' }, { ...disabled, name: 'old.example.com' }]));
        await mountPage();
        const notes = wrapper!.findAll('[data-testid="mx-instruction"]');
        expect(notes).toHaveLength(1);
        expect(notes[0]!.text()).toBe(`Set its MX record to ${MX_HOST}`);
        expect(notes[0]!.element.closest('tr')!.textContent).toContain('ok.example.com');
        wrapper!.unmount();
        api.listInboundDomains.mockResolvedValue(list([active], null));
        await mountPage();
        expect(wrapper!.find('[data-testid="mx-instruction"]').exists()).toBe(false);
    });

    it('lists an active domain with Disable, and a disabled one with Enable', async () => {
        api.listInboundDomains.mockResolvedValue(list([active, { ...disabled, name: 'old.example.com' }]));
        await mountPage();
        expect(wrapper!.text()).toContain('Verified');
        expect(wrapper!.text()).toContain('Disabled');
        expect(hasButton('Verify')).toBe(false);
        expect(hasButton('Disable')).toBe(true);
        expect(hasButton('Enable')).toBe(true);
    });

    it.each([
        ['thinpost', 'Points to Thinpost'],
        ['mixed', 'Partly Thinpost'],
        ['other', 'Points elsewhere'],
        ['none', 'No MX record'],
        ['null_mx', 'Refuses mail'],
        ['error', 'Check failed']
    ])('shows the %s MX result with its check time', async (status, label) => {
        api.listInboundDomains.mockResolvedValue(list([{ ...active, mx_status: status, mx_checked_at: CHECKED }]));
        await mountPage();
        const text = wrapper!.text();
        expect(text).toContain(label);
        expect(text).toContain('checked');
        expect(wrapper!.find(`time[datetime="${CHECKED}"]`).exists()).toBe(true);
        expect(text).not.toContain('Not checked');
    });

    it('explains every MX result behind the help button', async () => {
        api.listInboundDomains.mockResolvedValue(list([]));
        await mountPage();
        const help = await openHelp(wrapper!, 'receiving domains');
        for (const label of ['Points to Thinpost', 'Partly Thinpost', 'Points elsewhere', 'No MX record', 'Refuses mail', 'Check failed']) {
            expect(help).toContain(label);
        }
        expect(help).toContain('first application to verify');
    });

    it('checks the MX record and reloads the stored result', async () => {
        api.listInboundDomains
            .mockResolvedValueOnce(list([active]))
            .mockResolvedValue(list([{ ...active, mx_status: 'thinpost', mx_checked_at: CHECKED }]));
        api.checkInboundDomainMx.mockResolvedValue({ data: { ...active, mx_status: 'thinpost', mx_checked_at: CHECKED } });
        await mountPage();
        await button('Check MX').trigger('click');
        await flushPromises();
        expect(api.checkInboundDomainMx).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'in.example.com' } });
        expect(wrapper!.text()).toContain('Points to Thinpost');
        expect(wrapper!.text()).not.toContain('not receiving yet');
        expect(wrapper!.find('td .tag.success').exists()).toBe(true);
    });

    it('disables Check MX and says the mail server has not connected yet', async () => {
        api.listInboundDomains.mockResolvedValue({ data: { domains: [active], mx_host_status: 'not_reported' } });
        await mountPage();
        const note = wrapper!.get('[data-testid="no-mx-host"]').text();
        expect(note).toContain("hasn't connected yet");
        expect(note).toContain('ask your Thinpost administrator');
        expect(button('Check MX').attributes('disabled')).toBeDefined();
    });

    it('withholds the mail server name and says so when DNS does not confirm it', async () => {
        api.listInboundDomains.mockResolvedValue({ data: { domains: [active], mx_host_status: 'dns_unconfirmed' } });
        await mountPage();
        const note = wrapper!.get('[data-testid="no-mx-host"]').text();
        expect(note).toContain("can't confirm the mail server's name");
        expect(note).not.toMatch(/HELO|Edge|operator/i);
        expect(wrapper!.find('[data-testid="mx-instruction"]').exists()).toBe(false);
        expect(button('Check MX').attributes('disabled')).toBeDefined();
    });

    it('verifies a pending domain', async () => {
        api.listInboundDomains.mockResolvedValueOnce(list([pending])).mockResolvedValue(list([active]));
        api.verifyInboundDomain.mockResolvedValue({ data: active });
        await mountPage();
        await button('Verify').trigger('click');
        await flushPromises();
        expect(api.verifyInboundDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'in.example.com' } });
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'in.example.com verified' }));
        expect(hasButton('Verify')).toBe(false);
    });

    it('explains a verification conflict without HTTP numbers', async () => {
        api.listInboundDomains.mockResolvedValue(list([pending]));
        api.verifyInboundDomain.mockRejectedValue(httpError(409));
        await mountPage();
        await button('Verify').trigger('click');
        await flushPromises();
        const text = wrapper!.text();
        expect(text).toContain("doesn't match, or another application already owns this domain");
        expect(text).not.toContain('409');
    });

    it('explains a DNS lookup that is not ready yet', async () => {
        api.listInboundDomains.mockResolvedValue(list([pending]));
        api.verifyInboundDomain.mockRejectedValue(httpError(503));
        await mountPage();
        await button('Verify').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("Couldn't finish verifying. The TXT record may not be visible yet");
    });

    it('disables and enables a domain', async () => {
        api.listInboundDomains.mockResolvedValueOnce(list([active])).mockResolvedValueOnce(list([disabled])).mockResolvedValue(list([active]));
        api.disableInboundDomain.mockResolvedValue({ data: disabled });
        api.enableInboundDomain.mockResolvedValue({ data: active });
        await mountPage();
        await button('Disable').trigger('click');
        await flushPromises();
        expect(api.disableInboundDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'in.example.com' } });
        expect(wrapper!.text()).toContain('Disabled');
        await button('Enable').trigger('click');
        await flushPromises();
        expect(api.enableInboundDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'in.example.com' } });
    });

    it('deletes a domain only after confirmation', async () => {
        api.listInboundDomains.mockResolvedValueOnce(list([active])).mockResolvedValue(list([]));
        api.deleteInboundDomain.mockResolvedValue({});
        vf.showConfirm.mockResolvedValueOnce(false).mockResolvedValueOnce(true);
        await mountPage();
        await button('Delete').trigger('click');
        await flushPromises();
        expect(api.deleteInboundDomain).not.toHaveBeenCalled();
        await button('Delete').trigger('click');
        await flushPromises();
        expect(api.deleteInboundDomain).toHaveBeenCalledWith({ path: { appId: APP_ID, domain: 'in.example.com' } });
        expect(wrapper!.text()).toContain('No receiving domains');
    });

    it('shows the TXT record and the MX record after adding a domain, and does not store them', async () => {
        api.listInboundDomains.mockResolvedValueOnce(list([])).mockResolvedValue(list([pending]));
        api.requestInboundDomain.mockResolvedValue({ data: CHALLENGE });
        const setItem = vi.spyOn(Storage.prototype, 'setItem');
        await mountPage();
        await button('Add domain').trigger('click');
        await wrapper!.find('input[type="text"]').setValue('  In.Example.COM ');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.requestInboundDomain).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { domain: 'in.example.com' } });
        const values = wrapper!.findAll<HTMLInputElement>('input[readonly]').map(i => i.element.value);
        // TXT name and value, then the MX record: the domain and the mail server.
        expect(values).toEqual([CHALLENGE.dns_name, CHALLENGE.dns_value, 'in.example.com', MX_HOST]);
        expect(wrapper!.get('[data-testid="mx-note"]').text()).toContain('Until the domain is verified, Thinpost refuses mail for it and senders get it back');
        expect(wrapper!.text()).not.toContain('returned to the sender');
        expect(wrapper!.text()).toContain('only time the TXT value is shown');
        expect(setItem.mock.calls.flat().join(' ')).not.toContain(CHALLENGE.dns_value);
        setItem.mockRestore();
        // Done closes the records and the value is gone.
        await button('Done').trigger('click');
        expect(wrapper!.findAll('input[readonly]')).toHaveLength(0);
        expect(wrapper!.text()).not.toContain(CHALLENGE.dns_value);
        expect(wrapper!.text()).toContain('Not verified');
    });

    it('says there is no MX record to show until the mail server has connected', async () => {
        api.listInboundDomains.mockResolvedValue(list([], null));
        api.requestInboundDomain.mockResolvedValue({ data: { ...CHALLENGE, mx_host: undefined, mx_host_status: 'not_reported' } });
        await mountPage();
        await button('Add domain').trigger('click');
        await wrapper!.find('input[type="text"]').setValue('in.example.com');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(wrapper!.get('[data-testid="modal-no-mx-host"]').text()).toContain("hasn't connected yet");
        expect(wrapper!.findAll('input[readonly]')).toHaveLength(2);
    });

    it('rejects an invalid domain without calling the API', async () => {
        api.listInboundDomains.mockResolvedValue(list([]));
        await mountPage();
        await button('Add domain').trigger('click');
        await wrapper!.find('input[type="text"]').setValue('bücher.example');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.requestInboundDomain).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('xn-- form');
    });

    it('explains a refused domain name', async () => {
        api.listInboundDomains.mockResolvedValue(list([]));
        api.requestInboundDomain.mockRejectedValue(httpError(400));
        await mountPage();
        await button('Add domain').trigger('click');
        await wrapper!.find('input[type="text"]').setValue('bounce.example.com');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(wrapper!.text()).toContain("can't be used");
        expect(wrapper!.text()).not.toContain('400');
    });

    it('gets new records for a pending domain after confirmation', async () => {
        api.listInboundDomains.mockResolvedValue(list([pending]));
        api.requestInboundDomain.mockResolvedValue({ data: CHALLENGE });
        vf.showConfirm.mockResolvedValue(true);
        await mountPage();
        await button('New DNS records').trigger('click');
        await flushPromises();
        expect(vf.showConfirm).toHaveBeenCalled();
        expect(wrapper!.findAll<HTMLInputElement>('input[readonly]').map(i => i.element.value)).toContain(CHALLENGE.dns_value);
    });

    it('does not allow adding a domain to a paused application', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        api.listInboundDomains.mockResolvedValue(list([]));
        await mountPage();
        expect(button('Add domain').attributes('disabled')).toBeDefined();
        expect(wrapper!.text()).toContain('paused');
    });

    it('shows a load error with retry, and an unreachable API', async () => {
        api.listInboundDomains.mockRejectedValueOnce(networkError()).mockResolvedValue(list([]));
        await mountPage();
        expect(wrapper!.text()).toContain("Couldn't reach Thinpost");
        expect(wrapper!.find('table').exists()).toBe(false);
        expect(button('Add domain').attributes('disabled')).toBeDefined();
        await button('Try again').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('No receiving domains');
    });

    it('rejects a malformed listing instead of showing it', async () => {
        api.listInboundDomains.mockResolvedValue({ data: { domains: [{ name: 'in.example.com', state: 'bogus', created_at: 'x' }] } });
        await mountPage();
        expect(wrapper!.text()).toContain("didn't understand");
        expect(wrapper!.find('table').exists()).toBe(false);
    });
});

function limitInputs() {
    const inputs = wrapper!.findAll<HTMLInputElement>('.inbound-limits-modal input[type="text"]');
    if (inputs.length !== 2) throw new Error('limits dialog is not open');
    return inputs as [(typeof inputs)[number], (typeof inputs)[number]];
}

describe('inbound limits', () => {
    beforeEach(() => api.listInboundDomains.mockResolvedValue(list([])));

    it('shows the current limits', async () => {
        await mountPage();
        const text = wrapper!.get('[data-testid="inbound-limits"]').text();
        expect(text).toContain('2,000');
        expect(text).toContain('500');
    });

    it('shows an error when the limits cannot be loaded and does not offer editing', async () => {
        api.getInboundLimits.mockRejectedValue(httpError(503));
        await mountPage();
        expect(wrapper!.text()).toContain('Limits are unavailable right now');
        expect(button('Edit limits').attributes('disabled')).toBeDefined();
        expect(wrapper!.find('[data-testid="inbound-limits"]').exists()).toBe(false);
    });

    it('validates before saving and saves both values', async () => {
        api.setInboundLimits.mockResolvedValue({ data: { daily_limit: 5000, pending_limit: 250 } });
        await mountPage();
        await button('Edit limits').trigger('click');
        const [daily, pendingInput] = limitInputs();
        expect(button('Save limits').attributes('disabled')).toBeDefined();
        await daily.setValue('0');
        await daily.trigger('blur');
        expect(wrapper!.text()).toContain('Enter at least 1.');
        await daily.setValue('1000001');
        await daily.trigger('blur');
        expect(wrapper!.text()).toContain('Enter at most 1,000,000.');
        await daily.setValue('5000');
        await pendingInput.setValue('100001');
        await pendingInput.trigger('blur');
        expect(wrapper!.text()).toContain('Enter at most 100,000.');
        await wrapper!.find('form').trigger('submit');
        expect(api.setInboundLimits).not.toHaveBeenCalled();
        await pendingInput.setValue('250');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.setInboundLimits).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { daily_limit: 5000, pending_limit: 250 } });
        const text = wrapper!.get('[data-testid="inbound-limits"]').text();
        expect(text).toContain('5,000');
        expect(text).toContain('250');
    });

    it('shows a failed save in the dialog and keeps the old values', async () => {
        api.setInboundLimits.mockRejectedValue(httpError(404));
        await mountPage();
        await button('Edit limits').trigger('click');
        const [daily] = limitInputs();
        await daily.setValue('10');
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(wrapper!.text()).toContain('Nothing was changed');
        expect(wrapper!.get('[data-testid="inbound-limits"]').text()).toContain('2,000');
    });
});

const MSG_ID = 'cccccccccccccccccccccccccccccccc';
const OUTBOUND_ID = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';

function row(overrides: Record<string, unknown> = {}) {
    return {
        id: MSG_ID,
        received_at: '2026-11-13T10:00:00Z',
        from: { address: 'alice@example.org' },
        recipient: 'reply@in.example.com',
        recipient_count: 3,
        subject: 'Re: your receipt',
        enhanced_protection: false,
        size_bytes: 2048,
        spf: 'pass',
        dkim: 'pass',
        dmarc: 'pass',
        correlation: { matched: true, message_id: OUTBOUND_ID },
        webhook: { status: 'delivered', total: 3, delivered: 3, pending: 0, failed: 0 },
        content_expired: false,
        ...overrides
    };
}

describe('received messages', () => {
    beforeEach(() => {
        api.listInboundDomains.mockResolvedValue(list([active]));
    });

    it('lists received mail with its recipient count, DMARC result, reply badge and webhook state', async () => {
        api.listInboundMessages.mockResolvedValue({ data: { messages: [row()] } });
        await mountPage();
        expect(api.listInboundMessages).toHaveBeenCalledWith({ path: { appId: APP_ID }, query: { limit: 50 } });
        const line = wrapper!.get(`[data-inbound="${MSG_ID}"]`);
        expect(line.text()).toContain('alice@example.org');
        expect(line.text()).toContain('reply@in.example.com');
        expect(line.text()).toContain('+2');
        expect(line.text()).toContain('Re: your receipt');
        expect(line.text()).toContain('2.0 KiB');
        expect(line.text()).toContain('Pass');
        expect(line.text()).toContain('Reply');
        expect(line.text()).toContain('Delivered');
        expect(line.get('a').attributes('href')).toBe(`/apps/${APP_ID}/inbound/${MSG_ID}`);
        expect(line.text()).not.toContain('Expired');
    });

    it('says so when nothing has been received', async () => {
        await mountPage();
        expect(wrapper!.get('[data-testid="no-inbound-messages"]').text()).toContain('No messages received yet');
    });

    it('shows Protected instead of the subject of an Enhanced Protection message', async () => {
        api.listInboundMessages.mockResolvedValue({ data: { messages: [row({ enhanced_protection: true, subject: 'leaked' })] } });
        await mountPage();
        const line = wrapper!.get(`[data-inbound="${MSG_ID}"]`);
        expect(line.text()).toContain('Protected');
        expect(line.text()).not.toContain('leaked');
    });

    it('marks expired messages, and failed or partly delivered webhooks', async () => {
        api.listInboundMessages.mockResolvedValue({
            data: {
                messages: [
                    row({ content_expired: true, correlation: { matched: false }, subject: undefined, webhook: { status: 'failed', total: 1, delivered: 0, pending: 0, failed: 1 } }),
                    row({ id: 'dddddddddddddddddddddddddddddddd', webhook: { status: 'partial', total: 2, delivered: 1, pending: 0, failed: 1 }, dmarc: 'fail' })
                ]
            }
        });
        await mountPage();
        const first = wrapper!.get(`[data-inbound="${MSG_ID}"]`);
        expect(first.text()).toContain('Expired');
        expect(first.text()).toContain('Failed');
        expect(first.text()).toContain('(no subject)');
        expect(first.text()).not.toContain('Reply');
        const second = wrapper!.get('[data-inbound="dddddddddddddddddddddddddddddddd"]');
        expect(second.text()).toContain('Partly delivered');
        expect(second.text()).toContain('Fail');
    });

    it('pages like the outbound list', async () => {
        api.listInboundMessages
            .mockResolvedValueOnce({ data: { messages: [row()], next_cursor: 'cursor-1' } })
            .mockResolvedValueOnce({ data: { messages: [row({ id: 'dddddddddddddddddddddddddddddddd' })] } });
        await mountPage();
        await button('Load older messages').trigger('click');
        await flushPromises();
        expect(api.listInboundMessages).toHaveBeenLastCalledWith({ path: { appId: APP_ID }, query: { limit: 50, cursor: 'cursor-1' } });
        expect(wrapper!.findAll('[data-inbound]')).toHaveLength(2);
        expect(wrapper!.text()).toContain('end of history');
    });

    it('rejects a response that does not match and offers a retry', async () => {
        api.listInboundMessages.mockResolvedValue({ data: { messages: [{ id: MSG_ID }] } });
        await mountPage();
        expect(wrapper!.text()).toContain("didn't understand");
        api.listInboundMessages.mockResolvedValue({ data: { messages: [row()] } });
        await button('Try again').trigger('click');
        await flushPromises();
        expect(wrapper!.find(`[data-inbound="${MSG_ID}"]`).exists()).toBe(true);
    });

    it('sends no search or filter: the list call carries only paging', async () => {
        await mountPage();
        for (const call of api.listInboundMessages.mock.calls) {
            expect(Object.keys(call[0].query ?? {}).sort()).toEqual(['limit']);
        }
        expect(wrapper!.find('input[type="search"]').exists()).toBe(false);
    });
});

describe('add inbound webhook', () => {
    it('offers the action on the tab and in the warning, both opening the Webhooks page with the inbound format', async () => {
        api.listInboundDomains.mockResolvedValue(list([active]));
        api.listAppWebhooks.mockResolvedValue({ data: { webhooks: [OUTBOUND_HOOK] } });
        await mountPage();
        expect(wrapper!.get('[data-testid="add-inbound-webhook"]').attributes('href')).toBe(`/apps/${APP_ID}/webhooks?add=thinpost_inbound`);
        expect(wrapper!.get('[data-testid="add-inbound-webhook-warning"]').attributes('href')).toBe(`/apps/${APP_ID}/webhooks?add=thinpost_inbound`);
    });
});
