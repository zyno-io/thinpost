import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import { installVf } from '@zyno-io/vue-foundation';
import { createMemoryHistory, createRouter } from 'vue-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, openHelp } from './helpers';

const api = vi.hoisted(() => ({
    getOperations: vi.fn(),
    listOperationsAttention: vi.fn(),
    getRuntime: vi.fn(),
    setRetryPolicy: vi.fn(),
    getPublicConfig: vi.fn(),
    getFeedbackLoops: vi.fn(),
    listSendingIps: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showConfirmDestroy: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { default: Operations } = await import('@/screens/operations.vue');
const { formatAge, formatMs, formatRemaining, isStaleDeployClosure, parseOperations, validatePolicyDraft, draftFromPolicy } = await import('@/operations');

const APP_ID = '0123456789abcdef0123456789abcdef';
const MESSAGE_ID = 'fedcba9876543210fedcba9876543210';
const DELIVERY_ID = 'abababababababababababababababab';
const policy = { max_attempts: 128, initial_delay_ms: 1_800_000, max_delay_ms: 10_800_000, max_age_ms: 432_000_000, revision: 3, changed_at: '2026-11-10T10:00:00Z', changed_by: 'operator-1' };

function snapshot(overrides: Record<string, unknown> = {}) {
    // One clock reading: separate Date.now() calls can straddle a millisecond and floor 10 min to 9.
    const now = Date.now();
    return {
        generated_at: new Date(now).toISOString(),
        gate: { mode: 'open', revision: 9, changed_at: '2026-11-10T09:00:00Z', changed_by: 'deploy:ns/rel:r4', generation_matches: true, quarantine_complete: true },
        workers: [{ worker_id: 'edge-1', controller_id: 'control-a', epoch: 4, expires_at: new Date(now + 90_000).toISOString(), live: true }],
        messages: { queued_due: 2, queued_waiting: 1, preparing: 0, preparation_failed: 1, recovery_hold: 0, on_hold: 4, oldest_due_at: new Date(now - 600_000).toISOString() },
        deliveries: { queued_due: 5, queued_waiting: 3, in_flight: 1, uncertain: 2, recipient_policy_held: 1, tracking_held: 1, recovery_hold: 0 },
        webhooks: { queued_due: 0, queued_waiting: 0, in_flight: 0, recovery_hold: 0 },
        retry_policy: policy,
        ...overrides
    };
}
const attentionItem = { app_id: APP_ID, app_name: 'Billing', message_id: MESSAGE_ID, delivery_id: DELIVERY_ID, kind: 'uncertain', since: '2026-11-10T08:00:00Z' };

let wrapper: VueWrapper | undefined;

async function mountPage() {
    const router = createRouter({
        history: createMemoryHistory(),
        routes: [
            { path: '/operations', component: Operations },
            { path: '/apps/:appId/messages/:messageId', name: 'app-message', component: { template: '<div />' } }
        ]
    });
    await router.push('/operations');
    wrapper = mount({ template: '<RouterView />' }, { global: { plugins: [router, installVf] }, attachTo: document.body });
    await flushPromises();
}
const dialog = () => wrapper!.find('[role="dialog"]');
function button(label: string) {
    const match = wrapper!.findAll('button').find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    api.getOperations.mockResolvedValue({ data: snapshot() });
    api.listOperationsAttention.mockResolvedValue({ data: { items: [attentionItem] } });
    api.getFeedbackLoops.mockResolvedValue({ data: { receiving: true, reporters: [], counts: [] } });
    api.listSendingIps.mockResolvedValue({ data: { ips: [], unpinned_app_count: 0, edge_reported_at: null, edge_reports_sources: false, edge_primary_address: null, unnamed_edge_addresses: [] } });
    api.getRuntime.mockResolvedValue({ data: { features: { submission: true, tracking: false } } });
});

afterEach(() => {
    vi.useRealTimers();
    wrapper?.unmount();
    wrapper = undefined;
    document.body.innerHTML = '';
});

describe('operations helpers', () => {
    it('formats durations compactly', () => {
        expect(formatMs(1_800_000)).toBe('30 min');
        expect(formatMs(10_800_000)).toBe('3 h');
        expect(formatMs(5_400_000)).toBe('1 h 30 min');
        expect(formatMs(432_000_000)).toBe('5 d');
        expect(formatAge('2026-11-11T10:00:00Z', Date.parse('2026-11-11T10:00:20Z'))).toBe('<1 min');
        expect(formatAge('2026-11-11T10:00:00Z', Date.parse('2026-11-11T12:05:30Z'))).toBe('2 h 5 min');
        expect(formatRemaining('2026-11-11T10:00:00Z', Date.parse('2026-11-11T10:00:30Z'))).toBeNull();
        expect(formatRemaining('2026-11-11T10:01:30Z', Date.parse('2026-11-11T10:00:00Z'))).toBe('1 min');
    });

    it('flags only a deploy-owned closure older than 30 minutes', () => {
        const gate = (mode: string, by: string, minutes: number) => ({ mode, changed_by: by, changed_at: new Date(Date.now() - minutes * 60_000).toISOString() }) as never;
        expect(isStaleDeployClosure(gate('closed', 'deploy:ns/rel:r1', 31), Date.now())).toBe(true);
        expect(isStaleDeployClosure(gate('closed', 'deploy:ns/rel:r1', 10), Date.now())).toBe(false);
        expect(isStaleDeployClosure(gate('closed', 'alice', 120), Date.now())).toBe(false);
        expect(isStaleDeployClosure(gate('open', 'deploy:ns/rel:r1', 120), Date.now())).toBe(false);
    });

    it('rejects a malformed snapshot', () => {
        expect(parseOperations(snapshot())).toBeTruthy();
        for (const body of [null, '<html>', {}, snapshot({ gate: { mode: 'weird' } }), snapshot({ messages: { queued_due: -1 } }), snapshot({ retry_policy: {} })]) {
            expect(() => parseOperations(body)).toThrow("Thinpost sent back something the console didn't understand");
        }
    });

    it('validates the retry policy like the server', () => {
        const draft = draftFromPolicy(policy as never);
        expect(draft).toEqual({ maxAttempts: '128', initialMinutes: '30', maxDelayMinutes: '180', maxAgeMinutes: '7200' });
        expect(validatePolicyDraft(draft).value).toEqual({ max_attempts: 128, initial_delay_ms: 1_800_000, max_delay_ms: 10_800_000, max_age_ms: 432_000_000 });
        expect(validatePolicyDraft({ ...draft, maxAttempts: '0' }).errors.maxAttempts).toBeTruthy();
        expect(validatePolicyDraft({ ...draft, maxAttempts: '1001' }).errors.maxAttempts).toBeTruthy();
        expect(validatePolicyDraft({ ...draft, initialMinutes: '0.5' }).errors.initial).toContain('at least 1 min');
        expect(validatePolicyDraft({ ...draft, initialMinutes: '1441' }).errors.initial).toContain('at most 1 d');
        expect(validatePolicyDraft({ ...draft, maxDelayMinutes: '10' }).errors.maxDelay).toContain('at least the initial delay');
        expect(validatePolicyDraft({ ...draft, maxAgeMinutes: '59' }).errors.maxAge).toContain('at least 1 h');
        expect(validatePolicyDraft({ ...draft, maxAgeMinutes: '43201' }).errors.maxAge).toContain('at most 30 d');
        expect(validatePolicyDraft({ ...draft, initialMinutes: 'abc' }).value).toBeNull();
    });
});

describe('operations page', () => {
    it('accepts a server that predates message holds', async () => {
        const old = snapshot();
        delete (old.messages as Record<string, unknown>).on_hold;
        api.getOperations.mockResolvedValue({ data: old });
        await mountPage();
        expect(wrapper!.find('[data-queue="messages"]').exists()).toBe(true);
        expect(wrapper!.text()).not.toContain("couldn't read");
    });

    it('renders the snapshot with its sources', async () => {
        await mountPage();
        const text = wrapper!.text();
        expect(text).toContain('as of');
        expect(wrapper!.find('[data-testid="gate-tag"]').text()).toBe('Open');
        expect(text).toContain('Change #9');
        expect(text).toContain('edge-1');
        expect(text).toContain('Connected');
        const row = wrapper!.find('[data-queue="deliveries"]').text();
        expect(row).toContain('Deliveries');
        expect(row).toContain('5');
        expect(wrapper!.find('[data-queue="messages"]').text()).toContain('10 min');
        // Messages on hold by their app are counted in the Held column.
        expect(wrapper!.find('[data-queue="messages"] [title="On hold by their app until it releases or cancels them"]').text()).toBe('4');
        const facts = wrapper!.find('[data-testid="policy-facts"]').text();
        expect(facts).toContain('128');
        expect(facts).toContain('30 min');
        expect(facts).toContain('5 d');
        expect(await openHelp(wrapper!, 'sending workers')).toContain("doesn't prove it can reach the internet's mail servers");
        expect(wrapper!.find('[data-testid="runtime-features"]').text()).toContain('Tracking: off');
        // No address-like content anywhere on the page.
        expect(wrapper!.text()).not.toMatch(/@\w+\.\w+/);
    });

    it('warns about a stuck deploy closure and a generation mismatch', async () => {
        api.getOperations.mockResolvedValue({
            data: snapshot({
                gate: { mode: 'closed', revision: 9, changed_at: new Date(Date.now() - 45 * 60_000).toISOString(), changed_by: 'deploy:ns/rel:r4', generation_matches: false, quarantine_complete: true }
            })
        });
        await mountPage();
        expect(wrapper!.find('[data-testid="gate-tag"]').text()).toBe('Closed');
        expect(wrapper!.find('[data-testid="stale-closure"]').text()).toContain('The upgrade may be stuck');
        expect(wrapper!.find('[data-testid="generation-mismatch"]').exists()).toBe(true);
        expect(button('Edit').attributes('disabled')).toBeDefined();
        expect(wrapper!.text()).toContain('Editing is only possible while mail intake is open.');
    });

    it('shows the restore check tag only while mail intake is in recovery', async () => {
        const gate = { revision: 9, changed_at: '2026-11-10T09:00:00Z', changed_by: 'operator-1', generation_matches: true, quarantine_complete: false };
        api.getOperations.mockResolvedValue({ data: snapshot({ gate: { ...gate, mode: 'open' } }) });
        await mountPage();
        expect(wrapper!.text()).not.toContain('Restore check not completed');
        wrapper!.unmount();
        api.getOperations.mockResolvedValue({ data: snapshot({ gate: { ...gate, mode: 'recovery' } }) });
        await mountPage();
        expect(wrapper!.text()).toContain('Restore check not completed');
    });

    it('shows Disconnected for an expired lease and states when no worker is recorded', async () => {
        api.getOperations.mockResolvedValue({ data: snapshot({ workers: [{ worker_id: 'edge-1', controller_id: 'c', epoch: 1, expires_at: new Date(Date.now() - 120_000).toISOString(), live: false }] }) });
        await mountPage();
        expect(wrapper!.text()).toContain('Disconnected');
        expect(wrapper!.text()).toContain('expired 2 min ago');
        wrapper!.unmount();
        api.getOperations.mockResolvedValue({ data: snapshot({ workers: [] }) });
        await mountPage();
        expect(wrapper!.find('[data-testid="no-worker"]').text()).toContain('No sending worker has connected.');
    });

    it('shows a 503 as unavailable instead of stale or zero data', async () => {
        api.getOperations.mockRejectedValue(httpError(503));
        await mountPage();
        expect(wrapper!.find('[data-testid="ops-error"]').text()).toContain('Thinpost status is unavailable.');
        expect(wrapper!.find('[data-testid="queues"]').exists()).toBe(false);
        expect(wrapper!.find('[data-testid="gate-tag"]').exists()).toBe(false);
    });

    it('lists attention items with links to message detail and pages them', async () => {
        api.listOperationsAttention
            .mockResolvedValueOnce({ data: { items: [attentionItem], next_cursor: 'c1' } })
            .mockResolvedValueOnce({ data: { items: [{ ...attentionItem, message_id: 'cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd', delivery_id: undefined, kind: 'preparation_failed' }] } });
        await mountPage();
        const link = wrapper!.find('[data-testid="attention"] a');
        expect(link.attributes('href')).toBe(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-testid="attention"]').text()).toContain('Outcome unknown');
        await button('Load more').trigger('click');
        await flushPromises();
        expect(api.listOperationsAttention).toHaveBeenLastCalledWith({ query: { limit: 50, cursor: 'c1' } });
        expect(wrapper!.findAll('[data-testid="attention"] tbody tr')).toHaveLength(2);
        expect(wrapper!.text()).toContain('2 loaded · end');
    });

    it('says plainly when nothing needs attention', async () => {
        api.listOperationsAttention.mockResolvedValue({ data: { items: [] } });
        await mountPage();
        expect(wrapper!.find('[data-testid="attention-empty"]').exists()).toBe(true);
    });

    it('shows the runtime features as unavailable when the report fails', async () => {
        api.getRuntime.mockRejectedValue(httpError(503));
        await mountPage();
        expect(wrapper!.find('[data-testid="runtime-unavailable"]').text()).toContain('Unavailable');
    });

    it('refreshes every 30 seconds while visible and never lets a superseded read overwrite a saved policy', async () => {
        vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
        await mountPage();
        expect(api.getOperations).toHaveBeenCalledTimes(1);
        let release!: (v: unknown) => void;
        api.getOperations.mockImplementationOnce(() => new Promise(r => (release = r)));
        await vi.advanceTimersByTimeAsync(30_000);
        expect(api.getOperations).toHaveBeenCalledTimes(2);
        api.setRetryPolicy.mockResolvedValue({ data: { ...policy, max_attempts: 20, revision: 4 } });
        await button('Edit').trigger('click');
        await flushPromises();
        const input = dialog().find('input[type="text"]');
        await input.setValue('20');
        await dialog().find('input[type="checkbox"]').setValue(true);
        await dialog().find('form').trigger('submit');
        await flushPromises();
        // The older snapshot, read before the save, arrives afterwards and must be ignored.
        release({ data: snapshot() });
        await flushPromises();
        expect(wrapper!.text()).toContain('Change #4');
        expect(wrapper!.find('[data-testid="policy-facts"]').text()).toContain('20');
    });

    it('checks the revision the editor was opened with, even after a reload behind it', async () => {
        vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
        await mountPage();
        await button('Edit').trigger('click');
        await flushPromises();
        // Auto-refresh is suspended while the editor is open.
        await vi.advanceTimersByTimeAsync(60_000);
        expect(api.getOperations).toHaveBeenCalledTimes(1);
        api.setRetryPolicy.mockRejectedValue(httpError(409));
        await dialog().find('input[type="text"]').setValue('20');
        await dialog().find('input[type="checkbox"]').setValue(true);
        await dialog().find('form').trigger('submit');
        await flushPromises();
        expect(api.setRetryPolicy.mock.calls[0]![0].body.expected_revision).toBe(3);
        // A conflict closes the editor so a resubmit cannot overwrite the newer policy.
        expect(dialog().exists()).toBe(false);
        expect(api.getOperations).toHaveBeenCalledTimes(2);
        expect(wrapper!.text()).toContain('Someone else changed the retry schedule');
    });

    it('closes the editor consistently when the snapshot becomes unavailable', async () => {
        await mountPage();
        await button('Edit').trigger('click');
        await flushPromises();
        api.getOperations.mockRejectedValue(httpError(503));
        await button('Refresh').trigger('click');
        await flushPromises();
        expect(dialog().exists()).toBe(false);
        expect(wrapper!.find('[data-testid="ops-error"]').exists()).toBe(true);
    });

    it('does not stick on Loading… when a refresh lands during Load more, and keeps paged lists on auto-refresh', async () => {
        vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
        api.listOperationsAttention.mockResolvedValueOnce({ data: { items: [attentionItem], next_cursor: 'c1' } });
        await mountPage();
        let release!: (v: unknown) => void;
        api.listOperationsAttention.mockImplementationOnce(() => new Promise(r => (release = r)));
        await button('Load more').trigger('click');
        api.listOperationsAttention.mockResolvedValueOnce({ data: { items: [attentionItem], next_cursor: 'c1' } });
        await button('Refresh').trigger('click');
        await flushPromises();
        release({ data: { items: [] } });
        await flushPromises();
        expect(button('Load more').text()).toBe('Load more');
        // After a second page is loaded, auto-refresh leaves the attention list alone.
        api.listOperationsAttention.mockResolvedValueOnce({ data: { items: [{ ...attentionItem, message_id: 'cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd' }] } });
        await button('Load more').trigger('click');
        await flushPromises();
        const calls = api.listOperationsAttention.mock.calls.length;
        await vi.advanceTimersByTimeAsync(30_000);
        expect(api.listOperationsAttention.mock.calls.length).toBe(calls);
        expect(wrapper!.findAll('[data-testid="attention"] tbody tr')).toHaveLength(2);
    });

    it('judges lease liveness by the server clock, not the browser clock', async () => {
        const skew = 10 * 60_000; // browser clock 10 minutes ahead of the server
        const serverNow = Date.now() - skew;
        api.getOperations.mockResolvedValue({
            data: snapshot({
                generated_at: new Date(serverNow).toISOString(),
                workers: [{ worker_id: 'edge-1', controller_id: 'c', epoch: 1, expires_at: new Date(serverNow + 60_000).toISOString(), live: true }]
            })
        });
        await mountPage();
        expect(wrapper!.text()).toContain('Connected');
        expect(wrapper!.text()).not.toContain('Disconnected');
    });

    describe('editing the retry policy', () => {
        async function openEditor() {
            await button('Edit').trigger('click');
            await flushPromises();
            const input = dialog().find('input[type="text"]');
            await vi.waitFor(() => expect(document.activeElement).toBe(input.element));
            return input;
        }

        it('requires a change and confirmation, then sends the revision it loaded', async () => {
            api.setRetryPolicy.mockResolvedValue({ data: { ...policy, max_attempts: 20, revision: 4, changed_by: 'operator-1' } });
            await mountPage();
            const input = await openEditor();
            expect(dialog().text()).toContain('Only affects retries scheduled from now on');
            expect(dialog().text()).toContain('Existing retry times are unchanged');
            expect(dialog().text()).toContain('This is already the current schedule.');
            await input.setValue('20');
            expect(dialog().find('button[type="submit"]').attributes('disabled')).toBeDefined();
            await dialog().find('input[type="checkbox"]').setValue(true);
            await dialog().find('form').trigger('submit');
            await flushPromises();
            expect(api.setRetryPolicy).toHaveBeenCalledWith({
                body: { max_attempts: 20, initial_delay_ms: 1_800_000, max_delay_ms: 10_800_000, max_age_ms: 432_000_000, expected_revision: 3 }
            });
            expect(dialog().exists()).toBe(false);
            expect(wrapper!.find('[data-testid="policy-facts"]').text()).toContain('20');
            expect(wrapper!.text()).toContain('Change #4');
        });

        it('shows durations in human units and rejects invalid values without calling the API', async () => {
            await mountPage();
            const input = await openEditor();
            expect(dialog().text()).toContain('30 min');
            expect(dialog().text()).toContain('5 d');
            await input.setValue('0');
            await input.trigger('blur');
            expect(dialog().text()).toContain('Enter 1 to 1,000.');
            await dialog().find('input[type="checkbox"]').setValue(true);
            await dialog().find('form').trigger('submit');
            await flushPromises();
            expect(api.setRetryPolicy).not.toHaveBeenCalled();
        });

        it('reports a stale revision as a conflict and reloads', async () => {
            api.setRetryPolicy.mockRejectedValue(httpError(409));
            await mountPage();
            const input = await openEditor();
            await input.setValue('20');
            await dialog().find('input[type="checkbox"]').setValue(true);
            await dialog().find('form').trigger('submit');
            await flushPromises();
            expect(dialog().exists()).toBe(false);
            expect(wrapper!.text()).toContain('Someone else changed the retry schedule');
            expect(api.getOperations).toHaveBeenCalledTimes(2);
        });

        it('flags an uncertain outcome and tells the user to refresh', async () => {
            api.setRetryPolicy.mockRejectedValue(httpError(502));
            await mountPage();
            const input = await openEditor();
            await input.setValue('20');
            await dialog().find('input[type="checkbox"]').setValue(true);
            await dialog().find('form').trigger('submit');
            await flushPromises();
            expect(dialog().text()).toContain("We can't tell whether the change went through");
            await button('Cancel').trigger('click');
            await flushPromises();
            expect(wrapper!.text()).toContain('may or may not have gone through');
        });
    });
});

describe('inbound card', () => {
    const inbound = { day: '2026-11-13', accepted: 120, correlated: 45, duplicate: 3, tempfail_routing: 7, tempfail_limit: 2, tempfail_internal: 1 };

    it('shows today\'s counters', async () => {
        api.getOperations.mockResolvedValue({ data: snapshot({ inbound }) });
        await mountPage();
        const facts = wrapper!.get('[data-testid="inbound-today"]').text();
        expect(facts).toContain('Received120');
        expect(facts).toContain('Replies45');
        expect(facts).toContain('Repeats3');
        expect(facts).toContain('not ready7');
        expect(facts).toContain('over a limit2');
        expect(facts).toContain('Thinpost problem1');
        expect(wrapper!.text()).toContain('2026-11-13');
        expect(await openHelp(wrapper!, 'inbound mail today')).toContain('never addresses or content');
    });

    it('says the counts are unavailable instead of showing zeros when the server does not send them', async () => {
        await mountPage();
        expect(wrapper!.find('[data-testid="inbound-today"]').exists()).toBe(false);
        expect(wrapper!.find('[data-testid="inbound-unavailable"]').exists()).toBe(true);
    });

    it('rejects a snapshot whose inbound counters are malformed', async () => {
        api.getOperations.mockResolvedValue({ data: snapshot({ inbound: { ...inbound, accepted: 'many' } }) });
        await mountPage();
        expect(wrapper!.find('[data-testid="ops-error"]').exists()).toBe(true);
        expect(() => parseOperations(snapshot({ inbound: { ...inbound, day: 'today' } }))).toThrow();
    });
});
