import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import { installVf } from '@zyno-io/vue-foundation';
import { createMemoryHistory, createRouter } from 'vue-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listSendingIps: vi.fn(),
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
        address: '45.0.1.22',
        helo_name: 'mx.example.net',
        created_at: '2026-11-01T10:00:00Z',
        app_count: 1,
        is_primary: true,
        on_edge: true,
        ...overrides
    };
}
const hosting = (overrides: Record<string, unknown> = {}) => ip({ id: HOSTING_ID, address: '45.0.0.10', helo_name: 'smtp.hosting.example.org', app_count: 2, is_primary: false, ...overrides });
function pool(overrides: Record<string, unknown> = {}) {
    return {
        ips: [ip(), hosting()],
        unpinned_app_count: 3,
        edge_reported_at: '2026-11-12T10:00:00Z',
        edge_reports_identity: true,
        edge_primary_address: '45.0.1.22',
        ...overrides
    };
}
const HOSTING_LABEL = 'smtp.hosting.example.org (45.0.0.10)';
const MAIN_LABEL = 'mx.example.net (45.0.1.22)';

let wrapper: VueWrapper | undefined;
async function mountCard() {
    wrapper = mount(Card, { global: { plugins: [installVf] }, attachTo: document.body });
    await flushPromises();
}
const row = (address: string) => wrapper!.find(`tr[data-ip="${address}"]`);
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
    it('rejects a malformed list or DNS response', () => {
        expect(sending.parseSendingIps(pool())).toBeTruthy();
        for (const body of [null, '<html>', {}, pool({ ips: 'x' }), pool({ ips: [{ ...ip(), on_edge: 'yes' }] }), pool({ ips: [{ ...ip(), helo_name: 5 }] }), pool({ unpinned_app_count: -1 }), pool({ edge_reports_identity: 'yes' }), pool({ edge_reports_identity: undefined })]) {
            expect(() => sending.parseSendingIps(body)).toThrow("didn't understand");
        }
        expect(() => sending.parseDnsCheck({ reverse: 'ok', forward: 'ok', mx: 'ok', spf: 'bogus', reverse_names: [] })).toThrow();
        expect(() => sending.parseDnsCheck({ reverse: 'ok', forward: 'ok', helo_spf: 'ok', bounce_spf: 'ok', reverse_names: [] })).toThrow();
        expect(sending.parseDnsCheck({ reverse: 'ok', forward: 'ok', mx: 'ok', spf: 'ok', reverse_names: [] })).toBeTruthy();
    });

    it('checks the shape of an app\'s choice', () => {
        expect(sending.isAppSendingIp({ sending_ip_id: null, address: null, helo_name: null })).toBe(true);
        expect(sending.isAppSendingIp({ sending_ip_id: HOSTING_ID, address: '45.0.0.10', helo_name: 'smtp.hosting.example.org' })).toBe(true);
        for (const bad of [null, {}, { sending_ip_id: null }, { sending_ip_id: null, address: '45.0.0.10', helo_name: null }, { sending_ip_id: 'x', address: '45.0.0.10', helo_name: 'h.example' }, { sending_ip_id: HOSTING_ID, address: null, helo_name: null }, { sending_ip_id: HOSTING_ID, name: 'hosting' }]) {
            expect(sending.isAppSendingIp(bad), JSON.stringify(bad)).toBe(false);
        }
    });

    it('labels an IP by its HELO name and address', () => {
        expect(sending.ipLabel({ helo_name: 'mx.example.net', address: '45.0.1.22' })).toBe(MAIN_LABEL);
    });

    it('says how many apps are waiting when the sending worker no longer has an IP', () => {
        expect(sending.workerNote({ on_edge: true, app_count: 2 })).toBe('');
        expect(sending.workerNote({ on_edge: null, app_count: 2 })).toBe('');
        expect(sending.workerNote({ on_edge: false, app_count: 0 })).toBe('The sending worker no longer has this IP.');
        expect(sending.workerNote({ on_edge: false, app_count: 1 })).toContain('mail from 1 app is waiting. Move it to another IP');
        expect(sending.workerNote({ on_edge: false, app_count: 3 })).toContain('mail from 3 apps is waiting. Move them to another IP');
    });

    it('turns each failed check into something to ask DNS staff for', () => {
        const target = { address: '45.0.0.10', helo_name: 'smtp.hosting.example.org' };
        expect(sending.dnsHints(target, { reverse: 'ok', reverse_names: [], forward: 'ok', mx: 'ok', spf: 'ok' })).toEqual([]);
        expect(sending.dnsHints(target, { reverse: 'ok', reverse_names: [], forward: 'ok', mx: 'unavailable', spf: 'unavailable' })).toEqual([]);
        const hints = sending.dnsHints(target, { reverse: 'mismatch', reverse_names: ['x.example'], forward: 'missing', mx: 'missing', spf: 'mismatch' });
        expect(hints).toHaveLength(4);
        expect(hints[0]).toBe('Set the reverse DNS (PTR record) of 45.0.0.10 to smtp.hosting.example.org.');
        expect(hints[1]).toContain('Point smtp.hosting.example.org to 45.0.0.10');
        expect(hints[2]).toContain('MX record for smtp.hosting.example.org');
        expect(hints[3]).toContain('ip4:45.0.0.10');
    });
});

describe('sending IPs card', () => {
    it('lists each IP with its HELO name, the main IP, app counts and worker status', async () => {
        await mountCard();
        expect(row('45.0.1.22').find('[data-testid="main-tag"]').exists()).toBe(true);
        expect(row('45.0.1.22').text()).toContain('45.0.1.22');
        expect(row('45.0.1.22').text()).toContain('mx.example.net');
        // The main IP serves the apps that chose it and the ones that never chose.
        expect(row('45.0.1.22').findAll('td')[1]!.text()).toBe('4');
        expect(row('45.0.0.10').findAll('td')[1]!.text()).toBe('2');
        expect(row('45.0.0.10').find('[data-testid="main-tag"]').exists()).toBe(false);
        expect(row('45.0.0.10').find('[data-testid="on-worker"]').text()).toBe('Yes');
        expect(wrapper!.find('[data-testid="reported-at"]').exists()).toBe(true);
        const help = await openHelp(wrapper!, 'sending IPs');
        expect(help).toContain('never sent from a different address');
    });

    it('is read-only and says who to ask for a change', async () => {
        await mountCard();
        expect(wrapper!.find('[data-testid="read-only-note"]').text()).toBe(
            'These IPs and their names are set up on the sending worker and appear here when it connects. To add or change one, ask your Thinpost administrator.'
        );
        for (const gone of ['add-ip', 'edit-ip', 'delete-ip', 'name-ip', 'unnamed', 'spf', 'blocker']) expect(wrapper!.find(`[data-testid="${gone}"]`).exists(), gone).toBe(false);
        expect(wrapper!.findAll('button').map(b => b.text().trim())).toEqual(expect.not.arrayContaining(['Add IP', 'Edit', 'Delete']));
        expect(wrapper!.text()).not.toMatch(/bounce domain|Name it|Not named yet/i);
        // The name column and the bounce domain column are gone.
        expect(wrapper!.findAll('th[scope="col"]').map(h => h.text())).toEqual(['IP address', 'HELO name', 'Apps using it', 'On the sending worker', 'DNS']);
    });

    it('says how many apps are waiting when an IP is not on the sending worker, and when it is not known', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip(), hosting({ on_edge: false })] }) });
        await mountCard();
        expect(row('45.0.0.10').find('[data-testid="on-worker"]').text()).toBe('No');
        expect(wrapper!.find('tr[data-detail="45.0.0.10"] [data-testid="worker-note"]').text()).toContain('mail from 2 apps is waiting. Move them to another IP');
        wrapper!.unmount();
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip({ on_edge: null }), hosting({ on_edge: null })], edge_reports_identity: false, edge_reported_at: '2026-11-12T10:00:00Z', edge_primary_address: null }) });
        await mountCard();
        expect(row('45.0.0.10').find('[data-testid="on-worker"]').text()).toBe('Unknown');
        expect(wrapper!.find('[data-testid="no-sources"]').text()).toContain('isn\'t sending mail');
        wrapper!.unmount();
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [], edge_reported_at: null, edge_reports_identity: false, edge_primary_address: null }) });
        await mountCard();
        expect(wrapper!.find('[data-testid="no-report"]').text()).toBe('The sending worker hasn\'t connected yet, so its IPs aren\'t known.');
        expect(wrapper!.find('[data-testid="empty"]').exists()).toBe(true);
        expect(wrapper!.find('[data-testid="sending-ips"]').exists()).toBe(false);
    });

    it('runs the DNS check and explains the results with what to ask for', async () => {
        await mountCard();
        api.checkSendingIpDns.mockResolvedValue({ data: { reverse: 'ok', reverse_names: ['smtp.hosting.example.org'], forward: 'ok', mx: 'missing', spf: 'mismatch' } });
        await row('45.0.0.10').find('[data-testid="check-dns"]').trigger('click');
        await flushPromises();
        expect(api.checkSendingIpDns).toHaveBeenCalledWith({ path: { sendingIpId: HOSTING_ID } });
        expect(row('45.0.0.10').find('[data-testid="dns-summary"]').text()).toBe('Needs attention');
        const detail = wrapper!.find('tr[data-detail="45.0.0.10"]');
        const results = detail.find('[data-testid="dns-results"]').text();
        expect(results).toContain('Mail for the HELO name (bounces) reaches this IP');
        expect(results).toContain('The HELO name\'s SPF record lists this IP');
        expect(results).toContain('Not found');
        expect(results).toContain('Only ip4: and a entries are read');
        const hints = detail.find('[data-testid="dns-hints"]').text();
        expect(hints).toContain('MX record for smtp.hosting.example.org');
        expect(hints).toContain('v=spf1 ip4:45.0.0.10');
        expect(hints).not.toContain('reverse DNS');
    });

    it('shows no fix-it hints when every check passes', async () => {
        await mountCard();
        api.checkSendingIpDns.mockResolvedValue({ data: { reverse: 'ok', reverse_names: ['smtp.hosting.example.org'], forward: 'ok', mx: 'ok', spf: 'ok' } });
        await row('45.0.0.10').find('[data-testid="check-dns"]').trigger('click');
        await flushPromises();
        expect(row('45.0.0.10').find('[data-testid="dns-summary"]').text()).toBe('All good');
        expect(wrapper!.find('[data-testid="dns-hints"]').exists()).toBe(false);
    });

    it('reports a failed DNS check without hiding the list', async () => {
        await mountCard();
        api.checkSendingIpDns.mockRejectedValue(httpError(503));
        await row('45.0.0.10').find('[data-testid="check-dns"]').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('The DNS check is unavailable right now');
        expect(row('45.0.0.10').exists()).toBe(true);
    });

    it('shows an error instead of an empty list when loading fails', async () => {
        api.listSendingIps.mockRejectedValue(httpError(503));
        await mountCard();
        expect(wrapper!.text()).toContain('Sending IPs are temporarily unavailable');
        expect(wrapper!.find('[data-testid="sending-ips"]').exists()).toBe(false);
        expect(wrapper!.find('[data-testid="empty"]').exists()).toBe(false);
    });
});

describe('sending IP panel', () => {
    async function mountPanel(current: Record<string, unknown> = { sending_ip_id: null, address: null, helo_name: null }) {
        api.getAppSendingIp.mockResolvedValue({ data: current });
        const mounted = await mountAppRoute(`/apps/${APP_ID}`, { 'app-keys': Panel });
        wrapper = mounted.wrapper;
    }
    beforeEach(() => {
        api.listApps.mockResolvedValue({ data: { apps: [app] } });
    });

    it('shows the main IP and lets the person change it', async () => {
        await mountPanel();
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain('Main IP');
        const select = wrapper!.find('[data-testid="sending-ip-select"]');
        // The main IP's own row is the default choice, not a second entry.
        expect(select.findAll('option').map(o => o.text())).toEqual(['Main IP (default)', HOSTING_LABEL]);
        expect(wrapper!.find('[data-testid="save-sending-ip"]').attributes('disabled')).toBeDefined();
        await select.setValue(HOSTING_ID);
        api.setAppSendingIp.mockResolvedValue({ data: { sending_ip_id: HOSTING_ID, address: '45.0.0.10', helo_name: 'smtp.hosting.example.org' } });
        await wrapper!.find('[data-testid="save-sending-ip"]').trigger('click');
        await flushPromises();
        expect(api.setAppSendingIp).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { sending_ip_id: HOSTING_ID } });
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain('smtp.hosting.example.org');
        expect(wrapper!.text()).toContain('starting with its next delivery');
    });

    it('names the main IP an app with no choice sends from', async () => {
        await mountPanel();
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain(`Main IP: ${MAIN_LABEL}`);
        expect(wrapper!.find('[data-testid="worker-state"]').text()).toBe('Available');
    });

    it('sends null to go back to the main IP', async () => {
        await mountPanel({ sending_ip_id: HOSTING_ID, address: '45.0.0.10', helo_name: 'smtp.hosting.example.org' });
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain('hosting');
        await wrapper!.find('[data-testid="sending-ip-select"]').setValue('');
        api.setAppSendingIp.mockResolvedValue({ data: { sending_ip_id: null, address: null, helo_name: null } });
        await wrapper!.find('[data-testid="save-sending-ip"]').trigger('click');
        await flushPromises();
        expect(api.setAppSendingIp).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { sending_ip_id: null } });
    });

    it('warns that mail is waiting when the IP is not on the sending worker', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip(), hosting({ on_edge: false })] }) });
        await mountPanel({ sending_ip_id: HOSTING_ID, address: '45.0.0.10', helo_name: 'smtp.hosting.example.org' });
        expect(wrapper!.find('[data-testid="worker-state"]').text()).toBe('Mail is waiting');
        expect(wrapper!.find('[data-testid="worker-note"]').text()).toContain('never sent from a different address');
    });

    it('disables an IP the sending worker does not have, unless it is the one in use', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip(), hosting({ on_edge: false })] }) });
        await mountPanel();
        const option = wrapper!.find('[data-testid="sending-ip-select"] option:nth-child(2)');
        expect(option.text()).toBe(`${HOSTING_LABEL} (not on the sending worker)`);
        expect(option.attributes('disabled')).toBeDefined();
        wrapper!.unmount();
        await mountPanel({ sending_ip_id: HOSTING_ID, address: '45.0.0.10', helo_name: 'smtp.hosting.example.org' });
        expect(wrapper!.find('[data-testid="sending-ip-select"] option:nth-child(2)').attributes('disabled')).toBeUndefined();
    });

    it('explains a refusal when the sending worker does not have the IP', async () => {
        await mountPanel();
        await wrapper!.find('[data-testid="sending-ip-select"]').setValue(HOSTING_ID);
        api.setAppSendingIp.mockRejectedValue(httpError(409));
        await wrapper!.find('[data-testid="save-sending-ip"]').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('doesn\'t have that IP right now');
        expect(wrapper!.find('[data-testid="current-ip"]').text()).toContain('Main IP');
    });

    it('says so when there are no other IPs, and when the list cannot load', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip()] }) });
        await mountPanel();
        expect(wrapper!.find('[data-testid="no-ips"]').text()).toContain('uses the main IP');
        expect(wrapper!.find('[data-testid="no-ips"]').text()).toContain('ask your Thinpost administrator');
        wrapper!.unmount();
        api.listSendingIps.mockRejectedValue(httpError(503));
        await mountPanel({ sending_ip_id: HOSTING_ID, address: '45.0.0.10', helo_name: 'smtp.hosting.example.org' });
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
        api.getAppSendingIp.mockResolvedValue({ data: { sending_ip_id: null, address: null, helo_name: null } });
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
        expect(cells).toEqual(['Main IP', HOSTING_LABEL, 'Unknown']);
    });

    it('says Unknown for a pinned app when the IP list cannot load, without blocking the list', async () => {
        api.listSendingIps.mockRejectedValue(httpError(503));
        await mountApps();
        const cells = wrapper!.findAll('[data-testid="sending-ip-cell"]').map(c => c.text());
        expect(cells).toEqual(['Main IP', 'Unknown', 'Unknown']);
        expect(wrapper!.findAll('tbody tr')).toHaveLength(3);
    });

    it('hides the IP picker while there is no other IP, and when the list cannot load', async () => {
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

    it('offers the IPs the sending worker has, with the main IP as the default, and sends the choice', async () => {
        await mountApps();
        await button('Create app').trigger('click');
        await flushPromises();
        const picker = document.querySelector('[data-testid="sending-ip-picker"]') as HTMLSelectElement;
        expect([...picker.options].map(o => o.textContent)).toEqual(['Main IP (default)', HOSTING_LABEL]);
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

    it('leaves out an IP the sending worker no longer has', async () => {
        api.listSendingIps.mockResolvedValue({ data: pool({ ips: [ip(), hosting({ on_edge: false })] }) });
        await mountApps();
        await button('Create app').trigger('click');
        await flushPromises();
        expect(document.querySelector('[data-testid="sending-ip-picker"]')).toBeNull();
    });

    it('explains when the chosen IP is refused because the sending worker does not have it', async () => {
        await mountApps();
        await button('Create app').trigger('click');
        await flushPromises();
        const picker = document.querySelector('[data-testid="sending-ip-picker"]') as HTMLSelectElement;
        const input = document.querySelector('input[type="text"]') as HTMLInputElement;
        input.value = 'New';
        input.dispatchEvent(new Event('input'));
        picker.value = HOSTING_ID;
        picker.dispatchEvent(new Event('change'));
        api.createApp.mockRejectedValue(httpError(409));
        dialogButton('Create app').click();
        await flushPromises();
        expect(document.querySelector('[role="dialog"]')!.textContent).toContain('doesn\'t have that IP right now');
    });

    function button(label: string) {
        const match = wrapper!.findAll('button').find(b => b.text().includes(label));
        if (!match) throw new Error(`button "${label}" not found`);
        return match;
    }
});
