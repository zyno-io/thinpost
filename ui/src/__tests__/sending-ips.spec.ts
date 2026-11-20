import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import { installVf } from '@zyno-io/vue-foundation';
import { createMemoryHistory, createRouter } from 'vue-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listSendingIps: vi.fn(),
    createSendingIp: vi.fn(),
    updateSendingIp: vi.fn(),
    deleteSendingIp: vi.fn(),
    checkSendingIpDns: vi.fn(),
    getAppSendingIp: vi.fn(),
    setAppSendingIp: vi.fn(),
    listApps: vi.fn(),
    createApp: vi.fn(),
    listAppKeys: vi.fn(),
    getSubmissionRate: vi.fn(),
    getRecipientQuota: vi.fn(),
    setRecipientQuota: vi.fn(),
    getPendingQuota: vi.fn(),
    setPendingQuota: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showConfirmDestroy: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { default: Card } = await import('@/shared/components/sending-ips-card.vue');
const { default: Panel } = await import('@/shared/components/sending-ip-panel.vue');
const { default: AppKeys } = await import('@/screens/app-keys.vue');
const { default: Apps } = await import('@/screens/apps.vue');
const sending = await import('@/sending-ips');

const APP_ID = '0123456789abcdef0123456789abcdef';
const MAIN_ID = '11111111111111111111111111111111';
const HOSTING_ID = '22222222222222222222222222222222';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z', sending_ip_id: null };

function ip(overrides: Record<string, unknown> = {}) {
    return {
        id: MAIN_ID,
        name: 'app-mail',
        address: '45.0.1.22',
        helo_name: 'smtp.mx.example.net',
        bounce_domain: 'bounce.example.net',
        created_at: '2026-11-01T10:00:00Z',
        app_count: 1,
        is_primary: true,
        on_edge: true,
        bounce_on_edge: true,
        ...overrides
    };
}
const hosting = (overrides: Record<string, unknown> = {}) =>
    ip({ id: HOSTING_ID, name: 'hosting', address: '45.0.0.10', helo_name: 'smtp.hosting.example.org', bounce_domain: 'hosting.example.org', app_count: 2, is_primary: false, ...overrides });
function pool(overrides: Record<string, unknown> = {}) {
    return {
        ips: [ip(), hosting()],
        unpinned_app_count: 3,
        edge_reported_at: '2026-11-12T10:00:00Z',
        edge_reports_sources: true,
        edge_primary_address: '45.0.1.22',
        unnamed_edge_addresses: [],
        global_bounce_domain: 'bounce.example.net',
        ...overrides
    };
}

let wrapper: VueWrapper | undefined;
async function mountCard(gateMode: string | null = 'open') {
    wrapper = mount(Card, { props: { gateMode }, global: { plugins: [installVf] }, attachTo: document.body });
    await flushPromises();
}
const buttons = (label: string) => wrapper!.findAll('button').filter(b => b.text().includes(label));
function button(label: string) {
    const match = buttons(label)[0];
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}
const row = (name: string) => wrapper!.find(`tr[data-ip="${name}"]`);
const modalInput = (labelText: string) => {
    const label = [...document.querySelectorAll('label')].find(l => l.textContent?.trim() === labelText);
    if (!label) throw new Error(`no field ${labelText}`);
    return document.getElementById(label.getAttribute('for')!) as HTMLInputElement;
};
async function type(labelText: string, value: string) {
    const input = modalInput(labelText);
    input.value = value;
    input.dispatchEvent(new Event('input'));
    input.dispatchEvent(new Event('blur'));
    await flushPromises();
}
const dialogButton = (label: string) => [...document.querySelectorAll('[role="dialog"] button')].find(b => b.textContent?.includes(label)) as HTMLButtonElement;

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    api.listSendingIps.mockResolvedValue({ data: pool() });
    api.getSubmissionRate.mockResolvedValue({ data: { per_minute: 600, burst: 100 } });
    api.getRecipientQuota.mockResolvedValue({ data: { daily_limit: 1000, used: 120, day: '2026-11-19' } });
    api.getPendingQuota.mockResolvedValue({ data: { limit: 500, pending: 7, truncated: false } });
});
afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
    document.body.innerHTML = '';
});

describe('sending IP helpers', () => {
    it('validates input like the server', () => {
        expect(sending.validateIpName('app-mail')).toBeNull();
        for (const bad of ['', 'primary', 'other', 'removed', 'Hosting', '-x', 'x-', 'a b', 'a'.repeat(33), '45.0.1.22']) expect(sending.validateIpName(bad), bad).toBeTruthy();
        expect(sending.validateIpAddress('45.0.0.10')).toBeNull();
        for (const bad of ['', '45.0.0', '45.0.0.256', '045.0.0.1', '10.0.0.1', '127.0.0.1', '192.168.1.1', '172.16.0.1', '169.254.1.1', '224.0.0.1', '0.1.2.3', '2607:f8b0::1', 'mail.example.com']) {
            expect(sending.validateIpAddress(bad), bad).toBeTruthy();
        }
        expect(sending.validateIpDomain('smtp.example.org', 'HELO name', 253)).toBeNull();
        for (const bad of ['', 'localhost', 'SMTP.example.org', 'a b.example.org', '45.0.0.10', 'x.'.repeat(130)]) expect(sending.validateIpDomain(bad, 'HELO name', 253), bad).toBeTruthy();
        expect(sending.validateIpDomain('a'.repeat(60) + '.' + 'b'.repeat(60) + '.' + 'c'.repeat(60) + '.' + 'd'.repeat(40) + '.example', 'bounce domain', 219)).toBeTruthy();
    });

    it('requires the bounce domain to be the HELO name or a domain it belongs to', () => {
        expect(sending.validateBounceForHelo('smtp.example.org', 'smtp.example.org')).toBeNull();
        expect(sending.validateBounceForHelo('smtp.example.org', 'example.org')).toBeNull();
        expect(sending.validateBounceForHelo('smtp.example.org', 'mtp.example.org')).toBeTruthy();
        expect(sending.validateBounceForHelo('smtp.example.org', 'other.example')).toBeTruthy();
    });

    it('rejects a malformed pool or DNS response', () => {
        expect(sending.parseSendingIps(pool())).toBeTruthy();
        for (const body of [null, '<html>', {}, pool({ ips: 'x' }), pool({ ips: [{ ...ip(), on_edge: 'yes' }] }), pool({ ips: [{ ...ip(), name: 'Bad Name' }] }), pool({ unpinned_app_count: -1 }), pool({ edge_reports_sources: 'yes' })]) {
            expect(() => sending.parseSendingIps(body)).toThrow("didn't understand");
        }
        expect(() => sending.parseDnsCheck({ reverse: 'ok', forward: 'ok', helo_spf: 'ok', bounce_spf: 'bogus', reverse_names: [] })).toThrow();
        expect(sending.parseDnsCheck({ reverse: 'ok', forward: 'ok', helo_spf: 'ok', bounce_spf: 'ok', reverse_names: [] })).toBeTruthy();
    });

    it('groups IPs by bounce domain for SPF', () => {
        const groups = sending.spfGroups([ip(), hosting(), hosting({ id: '3'.repeat(32), name: 'hosting-2', address: '45.0.0.11' }), ip({ id: '4'.repeat(32), name: 'other', address: '45.0.1.23' })] as never);
        expect(groups).toEqual([
            { domain: 'bounce.example.net', addresses: ['45.0.1.22', '45.0.1.23'], entries: 'ip4:45.0.1.22 ip4:45.0.1.23' },
            { domain: 'hosting.example.org', addresses: ['45.0.0.10', '45.0.0.11'], entries: 'ip4:45.0.0.10 ip4:45.0.0.11' }
        ]);
        expect(sending.spfGroups([hosting({ helo_name: 'smtp.hosting.test', bounce_domain: 'smtp.hosting.test' })] as never)).toEqual([
            { domain: 'smtp.hosting.test', addresses: ['45.0.0.10'], entries: 'a' }
        ]);
    });

    it('explains why a delete is not possible', () => {
        expect(sending.deleteBlocker({ app_count: 0 }, 'open', true)).toBeNull();
        expect(sending.deleteBlocker({ app_count: 1 }, 'open', true)).toBe('1 app uses this IP. Move it to another IP first.');
        expect(sending.deleteBlocker({ app_count: 3 }, 'open', true)).toContain('3 apps use this IP');
        expect(sending.deleteBlocker({ app_count: 0 }, 'closed', true)).toContain('mail intake is open');
        expect(sending.deleteBlocker({ app_count: 0 }, 'open', false)).toContain('Sign in');
    });
});

describe('sending IPs card', () => {
    it('lists the pool with the main IP, app counts and worker status', async () => {
        await mountCard();
        expect(row('app-mail').find('[data-testid="main-tag"]').exists()).toBe(true);
        expect(row('app-mail').text()).toContain('45.0.1.22');
        expect(row('app-mail').text()).toContain('smtp.mx.example.net');
        expect(row('app-mail').text()).toContain('bounce.example.net');
        // The main IP serves the apps that chose it and the ones that never chose.
        expect(row('app-mail').findAll('td')[3]!.text()).toBe('4');
        expect(row('hosting').findAll('td')[3]!.text()).toBe('2');
        expect(row('hosting').find('[data-testid="main-tag"]').exists()).toBe(false);
        expect(row('hosting').find('[data-testid="on-worker"]').text()).toBe('Yes');
        expect(wrapper!.find('[data-testid="reported-at"]').exists()).toBe(true);
        const help = await openHelp(wrapper!, 'sending IPs');
        expect(help).toContain('never sent from a different address');
    });

    it('says when an address is not on the sending worker, and when it is not known', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip(), hosting({ on_edge: false, bounce_on_edge: false })] }) });
        await mountCard();
        expect(row('hosting').find('[data-testid="on-worker"]').text()).toBe('No');
        expect(wrapper!.find('tr[data-detail="hosting"] [data-testid="worker-note"]').text()).toContain('is never sent from another address');
        wrapper!.unmount();
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip({ on_edge: null, bounce_on_edge: null }), hosting({ on_edge: null, bounce_on_edge: null })], edge_reports_sources: false, edge_reported_at: '2026-11-12T10:00:00Z', edge_primary_address: null }) });
        await mountCard();
        expect(row('hosting').find('[data-testid="on-worker"]').text()).toBe('Unknown');
        expect(wrapper!.find('[data-testid="no-sources"]').text()).toContain('isn\'t known');
        wrapper!.unmount();
        api.listSendingIps.mockResolvedValue({ data: pool({ edge_reported_at: null, edge_reports_sources: false, edge_primary_address: null }) });
        await mountCard();
        expect(wrapper!.find('[data-testid="no-report"]').exists()).toBe(true);
    });

    it('shows an addresses the worker reported that has no name, and the empty state', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [], unnamed_edge_addresses: ['45.0.1.22', '45.0.0.10'], unpinned_app_count: 5 }) });
        await mountCard();
        const unnamed = wrapper!.findAll('[data-testid="unnamed"]');
        expect(unnamed).toHaveLength(2);
        expect(unnamed[0]!.text()).toContain('Main IP');
        expect(unnamed[0]!.text()).toContain('5');
        expect(unnamed[1]!.text()).not.toContain('Main IP');
        wrapper!.unmount();
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [], unnamed_edge_addresses: [] }) });
        await mountCard();
        expect(wrapper!.find('[data-testid="empty"]').text()).toContain('Every app sends from the main IP');
        expect(wrapper!.find('[data-testid="empty"]').text()).toContain('bounce.example.net');
        expect(wrapper!.find('[data-testid="sending-ips"]').exists()).toBe(false);
    });

    it('shows what to add to each bounce domain\'s SPF record', async () => {
        await mountCard();
        const hostingSpf = wrapper!.find('[data-spf-domain="hosting.example.org"]');
        expect(hostingSpf.text()).toContain('ip4:45.0.0.10');
        expect(hostingSpf.text()).not.toContain('45.0.1.22');
        expect(wrapper!.find('[data-spf-domain="bounce.example.net"]').text()).toContain('ip4:45.0.1.22');
    });

    it('disables delete with the reason, and allows it for an unused IP', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip(), hosting({ app_count: 0 })] }) });
        await mountCard();
        const mainDelete = row('app-mail').find('[data-testid="delete-ip"]');
        expect(mainDelete.attributes('disabled')).toBeDefined();
        expect(wrapper!.find('tr[data-detail="app-mail"] [data-testid="delete-reason"]').text()).toBe('Can\'t delete: 1 app uses this IP. Move it to another IP first.');
        const unusedDelete = row('hosting').find('[data-testid="delete-ip"]');
        expect(unusedDelete.attributes('disabled')).toBeUndefined();
        vf.showConfirmDestroy.mockResolvedValue(true);
        api.deleteSendingIp.mockResolvedValue({});
        await unusedDelete.trigger('click');
        await flushPromises();
        expect(api.deleteSendingIp).toHaveBeenCalledWith({ path: { sendingIpId: HOSTING_ID } });
        expect(api.listSendingIps).toHaveBeenCalledTimes(2);
    });

    it('warns, when deleting the main IP\'s record, what unassigned apps will use instead', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip({ app_count: 0 })] }) });
        await mountCard();
        vf.showConfirmDestroy.mockResolvedValue(false);
        await row('app-mail').find('[data-testid="delete-ip"]').trigger('click');
        await flushPromises();
        expect(vf.showConfirmDestroy.mock.calls[0]![1]).toContain('own hostname and the default bounce domain');
    });

    it('does not delete when the confirmation is declined', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [hosting({ app_count: 0, is_primary: false })] }) });
        await mountCard();
        vf.showConfirmDestroy.mockResolvedValue(false);
        await row('hosting').find('[data-testid="delete-ip"]').trigger('click');
        await flushPromises();
        expect(api.deleteSendingIp).not.toHaveBeenCalled();
    });

    it('blocks every change while mail intake is closed or the session ended', async () => {
        await mountCard('closed');
        expect(wrapper!.find('[data-testid="blocker"]').text()).toContain('mail intake is open');
        expect(wrapper!.find('[data-testid="add-ip"]').attributes('disabled')).toBeDefined();
        expect(row('hosting').find('[data-testid="edit-ip"]').attributes('disabled')).toBeDefined();
        expect(row('hosting').find('[data-testid="delete-ip"]').attributes('disabled')).toBeDefined();
        wrapper!.unmount();
        authState.status = 'signed-out' as never;
        await mountCard('open');
        expect(wrapper!.find('[data-testid="blocker"]').text()).toContain('Sign in');
    });

    it('adds an IP after validating the form', async () => {
        await mountCard();
        await wrapper!.find('[data-testid="add-ip"]').trigger('click');
        await flushPromises();
        await type('Name', 'Bad Name');
        await type('IP address', '10.0.0.1');
        await type('HELO name', 'localhost');
        await type('Bounce domain', 'Bounce.Example');
        dialogButton('Add IP').click();
        await flushPromises();
        expect(api.createSendingIp).not.toHaveBeenCalled();
        expect(document.querySelector('[role="dialog"]')!.textContent).toContain('Use 1 to 32 lowercase letters');
        await type('Name', 'third');
        await type('IP address', '45.0.0.11');
        await type('HELO name', 'smtp.third.example.org');
        await type('Bounce domain', 'third.example.org');
        api.createSendingIp.mockResolvedValue({ data: hosting({ id: '5'.repeat(32), name: 'third', address: '45.0.0.11', app_count: 0 }) });
        dialogButton('Add IP').click();
        await flushPromises();
        expect(api.createSendingIp).toHaveBeenCalledWith({ body: { name: 'third', address: '45.0.0.11', helo_name: 'smtp.third.example.org', bounce_domain: 'third.example.org' } });
        expect(vf.showToast).toHaveBeenCalled();
        expect(document.querySelector('[role="dialog"]')).toBeNull();
    });

    it('starts the bounce domain from the HELO name and keeps it under that name', async () => {
        await mountCard();
        await wrapper!.find('[data-testid="add-ip"]').trigger('click');
        await flushPromises();
        await type('HELO name', 'smtp.hosting.example');
        expect((document.querySelector('[role="dialog"]')!.querySelector('input[id$="-bounce"]') as HTMLInputElement).value).toBe('smtp.hosting.example');
        // Once someone chooses a bounce domain, the HELO name no longer overwrites it.
        await type('Bounce domain', 'other.example');
        await type('HELO name', 'smtp.hosting.example.net');
        expect((document.querySelector('[role="dialog"]')!.querySelector('input[id$="-bounce"]') as HTMLInputElement).value).toBe('other.example');
        await type('Name', 'hosting-2');
        await type('IP address', '45.0.0.12');
        dialogButton('Add IP').click();
        await flushPromises();
        expect(api.createSendingIp).not.toHaveBeenCalled();
        expect(document.querySelector('[role="dialog"]')!.textContent).toContain('mail server name, or a domain it belongs to');
        await type('Bounce domain', 'hosting.example.net');
        api.createSendingIp.mockResolvedValue({ data: hosting({ id: '6'.repeat(32), name: 'hosting-2', address: '45.0.0.12', app_count: 0 }) });
        dialogButton('Add IP').click();
        await flushPromises();
        expect(api.createSendingIp).toHaveBeenCalledWith({ body: { name: 'hosting-2', address: '45.0.0.12', helo_name: 'smtp.hosting.example.net', bounce_domain: 'hosting.example.net' } });
    });

    it('lets a row saved before the bounce-domain rule be renamed', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip(), hosting({ bounce_domain: 'bounce.hosting.example.org' })] }) });
        await mountCard();
        await row('hosting').find('[data-testid="edit-ip"]').trigger('click');
        await flushPromises();
        await type('Name', 'hosting-renamed');
        api.updateSendingIp.mockResolvedValue({ data: hosting({ name: 'hosting-renamed' }) });
        dialogButton('Save').click();
        await flushPromises();
        expect(api.updateSendingIp).toHaveBeenCalledWith({ path: { sendingIpId: HOSTING_ID }, body: { name: 'hosting-renamed' } });
        // Changing the bounce domain to one the HELO name is not under is still refused.
        api.updateSendingIp.mockClear();
        await row('hosting').find('[data-testid="edit-ip"]').trigger('click');
        await flushPromises();
        await type('Bounce domain', 'elsewhere.example.net');
        dialogButton('Save').click();
        await flushPromises();
        expect(api.updateSendingIp).not.toHaveBeenCalled();
    });

    it('keeps the form open and explains a duplicate', async () => {
        await mountCard();
        await wrapper!.find('[data-testid="add-ip"]').trigger('click');
        await flushPromises();
        await type('Name', 'hosting');
        await type('IP address', '45.0.0.10');
        await type('HELO name', 'smtp.hosting.example.org');
        await type('Bounce domain', 'hosting.example.org');
        api.createSendingIp.mockRejectedValue(httpError(409));
        dialogButton('Add IP').click();
        await flushPromises();
        expect(document.querySelector('[role="dialog"]')!.textContent).toContain('already has that name or address');
    });

    it('edits only the changed fields and never offers to change the address', async () => {
        await mountCard();
        await row('hosting').find('[data-testid="edit-ip"]').trigger('click');
        await flushPromises();
        expect(document.querySelector('[data-testid="fixed-address"]')!.textContent).toContain('45.0.0.10');
        expect(document.querySelector('[role="dialog"]')!.textContent).toContain('An address can\'t be changed');
        expect(dialogButton('Save').disabled).toBe(true);
        await type('HELO name', 'smtp2.hosting.example.org');
        api.updateSendingIp.mockResolvedValue({ data: hosting() });
        dialogButton('Save').click();
        await flushPromises();
        expect(api.updateSendingIp).toHaveBeenCalledWith({ path: { sendingIpId: HOSTING_ID }, body: { helo_name: 'smtp2.hosting.example.org' } });
    });

    it('names an address the worker reported without letting its address change', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [], unnamed_edge_addresses: ['45.0.0.10'] }) });
        await mountCard();
        await wrapper!.find('[data-testid="name-ip"]').trigger('click');
        await flushPromises();
        expect(document.querySelector('[data-testid="fixed-address"]')!.textContent).toContain('45.0.0.10');
        await type('Name', 'hosting');
        await type('HELO name', 'smtp.hosting.example.org');
        await type('Bounce domain', 'hosting.example.org');
        api.createSendingIp.mockResolvedValue({ data: hosting() });
        dialogButton('Add IP').click();
        await flushPromises();
        expect(api.createSendingIp.mock.calls[0]![0].body.address).toBe('45.0.0.10');
    });

    it('runs the DNS check and explains the results', async () => {
        await mountCard();
        api.checkSendingIpDns.mockResolvedValue({ data: { reverse: 'ok', reverse_names: ['smtp.hosting.example.org'], forward: 'ok', helo_spf: 'missing', bounce_spf: 'mismatch' } });
        await row('hosting').find('[data-testid="check-dns"]').trigger('click');
        await flushPromises();
        expect(api.checkSendingIpDns).toHaveBeenCalledWith({ path: { sendingIpId: HOSTING_ID } });
        expect(row('hosting').find('[data-testid="dns-summary"]').text()).toBe('Needs attention');
        const results = wrapper!.find('tr[data-detail="hosting"] [data-testid="dns-results"]').text();
        expect(results).toContain('The HELO name has an SPF record');
        expect(results).toContain('Not found');
        expect(results).toContain('Only ip4: and a entries are read');
    });

    it('reports a failed DNS check without hiding the pool', async () => {
        await mountCard();
        api.checkSendingIpDns.mockRejectedValue(httpError(503));
        await row('hosting').find('[data-testid="check-dns"]').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('The DNS check is unavailable right now');
        expect(row('hosting').exists()).toBe(true);
    });

    it('shows an error instead of an empty pool when loading fails', async () => {
        api.listSendingIps.mockRejectedValue(httpError(503));
        await mountCard();
        expect(wrapper!.text()).toContain('Sending IPs are temporarily unavailable');
        expect(wrapper!.find('[data-testid="sending-ips"]').exists()).toBe(false);
        expect(wrapper!.find('[data-testid="empty"]').exists()).toBe(false);
        expect(wrapper!.find('[data-testid="add-ip"]').attributes('disabled')).toBeDefined();
    });
});

describe('sending IP panel', () => {
    async function mountPanel(current: Record<string, unknown> = { sending_ip_id: null, name: null }) {
        api.getAppSendingIp.mockResolvedValue({ data: current });
        const mounted = await mountAppRoute(`/apps/${APP_ID}`, { 'app-keys': Panel });
        wrapper = mounted.wrapper;
    }
    beforeEach(() => {
        api.listApps.mockResolvedValue({ data: { apps: [app] } });
    });

    it('shows the main IP and lets the administrator change it', async () => {
        await mountPanel();
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain('Main IP');
        const select = wrapper!.find('[data-testid="sending-ip-select"]');
        // The main IP's own record is the default choice, not a second entry.
        expect(select.findAll('option').map(o => o.text())).toEqual(['Main IP (default)', 'hosting']);
        expect(wrapper!.find('[data-testid="save-sending-ip"]').attributes('disabled')).toBeDefined();
        await select.setValue(HOSTING_ID);
        api.setAppSendingIp.mockResolvedValue({ data: { sending_ip_id: HOSTING_ID, name: 'hosting' } });
        await wrapper!.find('[data-testid="save-sending-ip"]').trigger('click');
        await flushPromises();
        expect(api.setAppSendingIp).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { sending_ip_id: HOSTING_ID } });
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain('hosting');
        expect(wrapper!.text()).toContain('starting with its next delivery');
    });

    it('shows mail waiting for an unassigned app when the main IP\'s record is not fully on the sending worker', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip({ bounce_on_edge: false }), hosting()] }) });
        await mountPanel();
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain('Main IP');
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain('smtp.mx.example.net');
        expect(wrapper!.find('[data-testid="worker-state"]').text()).toBe('Mail is waiting');
        expect(wrapper!.find('[data-testid="worker-note"]').text()).toContain('bounce domain');
    });

    it('sends null to go back to the main IP', async () => {
        await mountPanel({ sending_ip_id: HOSTING_ID, name: 'hosting' });
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain('hosting');
        await wrapper!.find('[data-testid="sending-ip-select"]').setValue('');
        api.setAppSendingIp.mockResolvedValue({ data: { sending_ip_id: null, name: null } });
        await wrapper!.find('[data-testid="save-sending-ip"]').trigger('click');
        await flushPromises();
        expect(api.setAppSendingIp).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { sending_ip_id: null } });
    });

    it('warns that mail is waiting when the IP is not on the sending worker', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip(), hosting({ on_edge: false })] }) });
        await mountPanel({ sending_ip_id: HOSTING_ID, name: 'hosting' });
        expect(wrapper!.find('[data-testid="worker-state"]').text()).toBe('Mail is waiting');
        expect(wrapper!.find('[data-testid="worker-note"]').text()).toContain('never sent from another address');
    });

    it('says so when no other IPs are named, and when the list cannot load', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip()] }) });
        await mountPanel();
        expect(wrapper!.find('[data-testid="no-ips"]').text()).toContain('uses the main IP');
        wrapper!.unmount();
        api.listSendingIps.mockRejectedValue(httpError(503));
        await mountPanel({ sending_ip_id: HOSTING_ID, name: 'hosting' });
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain('hosting');
        expect(wrapper!.find('[data-testid="pool-unavailable"]').exists()).toBe(true);
        expect(wrapper!.find('[data-testid="sending-ip-select"]').exists()).toBe(false);
    });

    it('explains a failed change and keeps the old value', async () => {
        await mountPanel();
        await wrapper!.find('[data-testid="sending-ip-select"]').setValue(HOSTING_ID);
        api.setAppSendingIp.mockRejectedValue(httpError(503));
        await wrapper!.find('[data-testid="save-sending-ip"]').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('Changing the sending IP is unavailable right now');
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain('Main IP');
    });

    it('shows an error instead of a guess when the assignment cannot load', async () => {
        api.getAppSendingIp.mockRejectedValue(httpError(503));
        const mounted = await mountAppRoute(`/apps/${APP_ID}`, { 'app-keys': Panel });
        wrapper = mounted.wrapper;
        expect(wrapper!.text()).toContain('temporarily unavailable');
        expect(wrapper!.find('[data-testid="current-ip"]').exists()).toBe(false);
    });

    it('appears on the app screen', async () => {
        api.getAppSendingIp.mockResolvedValue({ data: { sending_ip_id: null, name: null } });
        api.listAppKeys.mockResolvedValue({ data: { keys: [] } });
        const mounted = await mountAppRoute(`/apps/${APP_ID}`, { 'app-keys': AppKeys });
        wrapper = mounted.wrapper;
        expect(wrapper!.find('#sending-ip-heading').exists()).toBe(true);
    });
});

describe('apps list and create form', () => {
    async function mountApps() {
        const router = createRouter({
            history: createMemoryHistory(),
            routes: [
                { path: '/apps', name: 'apps', component: Apps },
                { path: '/apps/:appId', name: 'app-keys', component: { template: '<div>keys</div>' } },
                { path: '/operations', name: 'operations', component: { template: '<div>ops</div>' } }
            ]
        });
        await router.push('/apps');
        wrapper = mount({ template: '<RouterView />' }, { global: { plugins: [router, installVf] }, attachTo: document.body });
        await flushPromises();
        return router;
    }
    beforeEach(() => {
        api.listApps.mockResolvedValue({ data: { apps: [app, { ...app, id: 'a'.repeat(32), name: 'Hosted', sending_ip_id: HOSTING_ID }, { ...app, id: 'b'.repeat(32), name: 'Gone', sending_ip_id: '9'.repeat(32) }] } });
    });

    it('has a Sending IP column that says Main IP, the name, or Unknown', async () => {
        await mountApps();
        const cells = wrapper!.findAll('[data-testid="sending-ip-cell"]').map(c => c.text());
        expect(cells).toEqual(['Main IP', 'hosting', 'Unknown']);
    });

    it('says Unknown for a pinned app when the IP list cannot load, without blocking the list', async () => {
        api.listSendingIps.mockRejectedValue(httpError(503));
        await mountApps();
        const cells = wrapper!.findAll('[data-testid="sending-ip-cell"]').map(c => c.text());
        expect(cells).toEqual(['Main IP', 'Unknown', 'Unknown']);
        expect(wrapper!.findAll('tbody tr')).toHaveLength(3);
    });

    it('hides the IP picker while no IPs are named, and when the list cannot load', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip()] }) });
        await mountApps();
        await button('Create app').trigger('click');
        await flushPromises();
        expect(document.querySelector('[data-testid="sending-ip-picker"]')).toBeNull();
        api.createApp.mockResolvedValue({ data: { ...app, id: 'c'.repeat(32), name: 'New' } });
        const input = document.querySelector('input[type="text"]') as HTMLInputElement;
        input.value = 'New';
        input.dispatchEvent(new Event('input'));
        dialogButton('Create app').click();
        await flushPromises();
        expect(api.createApp).toHaveBeenCalledWith({ body: { name: 'New' } });
    });

    it('offers the named IPs, with the main IP as the default, and sends the choice', async () => {
        await mountApps();
        await button('Create app').trigger('click');
        await flushPromises();
        const picker = document.querySelector('[data-testid="sending-ip-picker"]') as HTMLSelectElement;
        expect([...picker.options].map(o => o.textContent)).toEqual(['Main IP (default)', 'hosting']);
        const input = document.querySelector('input[type="text"]') as HTMLInputElement;
        input.value = 'New';
        input.dispatchEvent(new Event('input'));
        picker.value = HOSTING_ID;
        picker.dispatchEvent(new Event('change'));
        api.createApp.mockResolvedValue({ data: { ...app, id: 'c'.repeat(32), name: 'New', sending_ip_id: HOSTING_ID } });
        dialogButton('Create app').click();
        await flushPromises();
        expect(api.createApp).toHaveBeenCalledWith({ body: { name: 'New', sending_ip_id: HOSTING_ID } });
    });

    function button(label: string) {
        const match = wrapper!.findAll('button').find(b => b.text().includes(label));
        if (!match) throw new Error(`button "${label}" not found`);
        return match;
    }
});
