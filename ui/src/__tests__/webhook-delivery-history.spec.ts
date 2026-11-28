import { flushPromises, mount, type DOMWrapper, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, networkError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppWebhooks: vi.fn(),
    listWebhookDeliveries: vi.fn(),
    listWebhookDeliveryAttempts: vi.fn(),
    replayWebhookDelivery: vi.fn(),
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
const { default: AppWebhooks } = await import('@/screens/app-webhooks.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const SUB_READY = '11111111111111111111111111111111';
const SUB_DISABLED = '33333333333333333333333333333333';
const MESSAGE_ID = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
const EVENT_FAILED = 'e0000000000000000000000000000001';
const EVENT_DELIVERED = 'e0000000000000000000000000000002';
const EVENT_QUEUED = 'e0000000000000000000000000000003';
const EVENT_OLDER = 'e0000000000000000000000000000004';
const READY_ENDPOINT = 'https://hooks.example.com/ready';

const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const webhooks = [
    { id: SUB_READY, format: 'thinpost', endpoint: READY_ENDPOINT, description: '', active: true, created_at: '2026-10-01T12:00:00Z', secret_created_at: '2026-10-01T12:00:00Z' },
    {
        id: SUB_DISABLED,
        format: 'thinpost',
        endpoint: 'https://hooks.example.com/disabled',
        description: '',
        active: false,
        created_at: '2026-10-01T12:00:00Z',
        secret_created_at: '2026-10-01T12:00:00Z'
    }
];

function delivery(eventId: string, subscriptionId: string, state: string, extra: Record<string, unknown> = {}) {
    return {
        event_id: eventId,
        subscription_id: subscriptionId,
        message_id: MESSAGE_ID,
        outcome: 'rejected',
        state,
        attempts: 3,
        occurred_at: '2026-11-01T10:00:00Z',
        ...extra
    };
}

const failed = delivery(EVENT_FAILED, SUB_READY, 'failed', { attempts: 12 });
const delivered = delivery(EVENT_DELIVERED, SUB_READY, 'delivered', { outcome: 'accepted', attempts: 1, delivered_at: '2026-11-01T10:00:05Z' });
const queued = delivery(EVENT_QUEUED, SUB_READY, 'queued', { next_attempt_at: '2026-11-01T10:05:00Z' });

let wrapper: VueWrapper | undefined;

async function mountAt(path = `/apps/${APP_ID}/webhooks`) {
    const mounted = await mountAppRoute(path, { 'app-webhooks': AppWebhooks });
    wrapper = mounted.wrapper;
    return wrapper;
}

function history() {
    return wrapper!.find('section[aria-labelledby="deliveries-heading"]');
}

function deliveryRow(eventId: string, subscriptionId = SUB_READY) {
    return history().find(`tr[data-delivery="${eventId}-${subscriptionId}"]`);
}

function buttonIn(scope: DOMWrapper<Element> | VueWrapper, label: string) {
    const match = scope.findAll('button').find(b => b.text().startsWith(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}

function hasButton(scope: DOMWrapper<Element>, label: string) {
    return scope.findAll('button').some(b => b.text().startsWith(label));
}

function lists(w: unknown = webhooks) {
    api.listAppWebhooks.mockResolvedValue({ data: { webhooks: w } });
}

function deliveries(items: unknown[], next_cursor?: string) {
    api.listWebhookDeliveries.mockResolvedValue({ data: next_cursor ? { deliveries: items, next_cursor } : { deliveries: items } });
}

const replayed = { data: undefined, response: new Response(null, { status: 204 }) };

async function openReplay(eventId = EVENT_FAILED, subscriptionId = SUB_READY) {
    await buttonIn(deliveryRow(eventId, subscriptionId), 'Send again').trigger('click');
    await flushPromises();
}

async function acknowledgeAndSubmit() {
    await wrapper!.find('input[type="checkbox"]').setValue(true);
    await wrapper!.find('form').trigger('submit');
    await flushPromises();
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    lists();
    deliveries([]);
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('webhook delivery history', () => {
    it('pages through history with the server cursor and shows delivery metadata only', async () => {
        api.listWebhookDeliveries
            .mockResolvedValueOnce({
                data: {
                    deliveries: [
                        { ...failed, claim_token: 'claim-hidden', endpoint: 'https://leak.example.com', payload: 'body-hidden', secret: 'whsec_hidden' },
                        delivered,
                        queued
                    ],
                    next_cursor: 'cursor-1'
                }
            })
            .mockResolvedValueOnce({ data: { deliveries: [delivery(EVENT_OLDER, SUB_READY, 'delivered'), delivered] } });
        await mountAt();

        expect(api.listWebhookDeliveries).toHaveBeenCalledWith({ path: { appId: APP_ID }, query: { limit: 50 } });
        expect(deliveryRow(EVENT_FAILED).text()).toContain('Failed');
        expect(deliveryRow(EVENT_FAILED).text()).toContain('rejected');
        expect(deliveryRow(EVENT_FAILED).text()).toContain('12');
        // The endpoint comes from the subscription list, not from the history response.
        expect(deliveryRow(EVENT_FAILED).text()).toContain(READY_ENDPOINT);
        expect(deliveryRow(EVENT_DELIVERED).text()).toContain('Delivered');
        expect(deliveryRow(EVENT_QUEUED).text()).toContain('Queued');
        expect(deliveryRow(EVENT_QUEUED).text()).toContain('not before');
        expect(deliveryRow(EVENT_FAILED).find(`a[href="/apps/${APP_ID}/messages/${MESSAGE_ID}"]`).exists()).toBe(true);

        const html = document.body.innerHTML;
        for (const hidden of ['claim-hidden', 'leak.example.com', 'body-hidden', 'whsec_hidden']) expect(html).not.toContain(hidden);
        expect(history().text()).toContain('3 events loaded');

        await buttonIn(history(), 'Load older events').trigger('click');
        await flushPromises();
        expect(api.listWebhookDeliveries).toHaveBeenLastCalledWith({ path: { appId: APP_ID }, query: { limit: 50, cursor: 'cursor-1' } });
        expect(deliveryRow(EVENT_OLDER).exists()).toBe(true);
        // A row repeated across pages is shown once.
        expect(history().findAll(`tr[data-delivery="${EVENT_DELIVERED}-${SUB_READY}"]`)).toHaveLength(1);
        expect(history().text()).toContain('4 events loaded · end of history');
        expect(hasButton(history(), 'Load older events')).toBe(false);
    });

    it('keeps loaded rows and the cursor when an older page fails', async () => {
        api.listWebhookDeliveries.mockResolvedValueOnce({ data: { deliveries: [failed], next_cursor: 'cursor-1' } }).mockRejectedValueOnce(networkError());
        await mountAt();
        await buttonIn(history(), 'Load older events').trigger('click');
        await flushPromises();
        expect(history().text()).toContain("Couldn't reach Thinpost");
        expect(deliveryRow(EVENT_FAILED).exists()).toBe(true);
        expect(hasButton(history(), 'Load older events')).toBe(true);
    });

    it('shows an empty history as empty', async () => {
        await mountAt();
        expect(history().text()).toContain('No webhook events yet');
        expect(history().find('table').exists()).toBe(false);
    });

    it('reports an unavailable history instead of an empty one, and retries', async () => {
        api.listWebhookDeliveries.mockRejectedValueOnce(httpError(503));
        await mountAt();
        expect(history().text()).toContain('Webhook event history is unavailable right now');
        expect(history().text()).not.toContain('No webhook events yet');
        // The subscription list is independent of the history.
        expect(wrapper!.text()).toContain(READY_ENDPOINT);

        deliveries([failed]);
        await buttonIn(history(), 'Try again').trigger('click');
        await flushPromises();
        expect(deliveryRow(EVENT_FAILED).exists()).toBe(true);
    });

    it('explains a 404 as a missing app or a server without webhook history', async () => {
        api.listWebhookDeliveries.mockRejectedValue(httpError(404));
        await mountAt();
        expect(history().text()).toContain("webhook event history isn't available");
    });

    it('rejects a response that does not match the contract', async () => {
        deliveries([{ ...failed, state: 'exploded' }]);
        await mountAt();
        expect(history().text()).toContain("didn't understand");
        expect(history().find('table').exists()).toBe(false);
    });

    it('explains that Queued is not a promise of immediate delivery', async () => {
        deliveries([queued]);
        await mountAt();
        expect(history().text()).not.toContain('mail admission');
        const text = await openHelp(wrapper!, 'webhook event history');
        expect(text).toContain('“Queued” means the event is waiting to be sent');
        expect(text).toContain("isn't a promise that it will arrive right away");
        expect(text).not.toContain('dispatcher');
        expect(text).toContain('recorded in the audit log');
    });

    it('blocks replay when webhook administration is unavailable', async () => {
        api.listAppWebhooks.mockRejectedValue(httpError(503));
        deliveries([failed]);
        await mountAt();
        expect(history().text()).toContain("Webhooks are unavailable right now, so events can't be sent again.");
        expect(buttonIn(deliveryRow(EVENT_FAILED), 'Send again').attributes('disabled')).toBeDefined();
        expect(deliveryRow(EVENT_FAILED).text()).toContain('Webhooks are unavailable right now.');
    });
});

describe('webhook delivery attempts', () => {
    it('shows the latest attempts on request and says when the list is truncated', async () => {
        deliveries([failed]);
        const attempts = Array.from({ length: 50 }, (_, i) => ({
            state: i === 0 ? 'failed' : i === 1 ? 'unknown' : 'failed',
            http_status: i === 0 ? 0 : i === 1 ? undefined : 500,
            started_at: '2026-11-01T10:00:00Z',
            finished_at: i === 1 ? undefined : '2026-11-01T10:00:01Z',
            id: `attempt-id-hidden-${i}`,
            claim_token: 'claim-hidden'
        }));
        api.listWebhookDeliveryAttempts.mockResolvedValue({ data: { attempts, truncated: true } });
        await mountAt();
        expect(api.listWebhookDeliveryAttempts).not.toHaveBeenCalled();

        const toggle = buttonIn(deliveryRow(EVENT_FAILED), 'Attempts');
        expect(toggle.attributes('aria-expanded')).toBe('false');
        await toggle.trigger('click');
        await flushPromises();

        expect(api.listWebhookDeliveryAttempts).toHaveBeenCalledWith({
            path: { appId: APP_ID, eventId: EVENT_FAILED, subscriptionId: SUB_READY }
        });
        const region = history().find('[role="region"]');
        expect(region.findAll('tbody tr')).toHaveLength(50);
        expect(region.text()).toContain('Showing only the latest 50 attempts.');
        expect(region.text()).not.toContain('operator API');
        expect(region.text()).toContain('0 · no response');
        expect(region.text()).toContain('Unknown');
        expect(region.text()).toContain('Not recorded');
        expect(document.body.innerHTML).not.toContain('attempt-id-hidden');
        expect(document.body.innerHTML).not.toContain('claim-hidden');

        await buttonIn(deliveryRow(EVENT_FAILED), 'Hide attempts').trigger('click');
        expect(history().find('[role="region"]').exists()).toBe(false);
    });

    it('says all attempts are shown when the list is not truncated, and shows an empty list as empty', async () => {
        deliveries([failed, queued]);
        api.listWebhookDeliveryAttempts
            .mockResolvedValueOnce({ data: { attempts: [{ state: 'succeeded', http_status: 204, started_at: '2026-11-01T10:00:00Z' }], truncated: false } })
            .mockResolvedValueOnce({ data: { attempts: [], truncated: false } });
        await mountAt();

        await buttonIn(deliveryRow(EVENT_FAILED), 'Attempts').trigger('click');
        await flushPromises();
        expect(history().find('[role="region"]').text()).not.toContain('Latest');
        expect(history().find('[role="region"]').findAll('tbody tr')).toHaveLength(1);

        await buttonIn(deliveryRow(EVENT_QUEUED), 'Attempts').trigger('click');
        await flushPromises();
        expect(history().findAll('[role="region"]')).toHaveLength(1);
        expect(history().find('[role="region"]').text()).toContain('No attempts recorded yet');
    });

    it('reports a failed attempt load with a retry', async () => {
        deliveries([failed]);
        api.listWebhookDeliveryAttempts.mockRejectedValueOnce(httpError(404)).mockResolvedValueOnce({ data: { attempts: [], truncated: false } });
        await mountAt();
        await buttonIn(deliveryRow(EVENT_FAILED), 'Attempts').trigger('click');
        await flushPromises();
        expect(history().text()).toContain("This event wasn't found for this app.");

        await buttonIn(history().find('[role="region"]'), 'Try again').trigger('click');
        await flushPromises();
        expect(history().text()).toContain('No attempts recorded yet');
    });

    it('rejects more than 50 attempts or a missing truncated flag', async () => {
        deliveries([failed]);
        const tooMany = Array.from({ length: 51 }, () => ({ state: 'failed', http_status: 500, started_at: '2026-11-01T10:00:00Z' }));
        api.listWebhookDeliveryAttempts
            .mockResolvedValueOnce({ data: { attempts: [] } })
            .mockResolvedValueOnce({ data: { attempts: tooMany, truncated: true } });
        await mountAt();
        await buttonIn(deliveryRow(EVENT_FAILED), 'Attempts').trigger('click');
        await flushPromises();
        expect(history().find('[role="region"]').text()).toContain("didn't understand");

        await buttonIn(history().find('[role="region"]'), 'Try again').trigger('click');
        await flushPromises();
        expect(api.listWebhookDeliveryAttempts).toHaveBeenCalledTimes(2);
        expect(history().find('[role="region"]').text()).toContain("didn't understand");
        expect(history().find('[role="region"] table').exists()).toBe(false);
    });
});

describe('webhook delivery replay', () => {
    it('offers replay only for failed deliveries', async () => {
        deliveries([failed, delivered, queued, delivery(EVENT_OLDER, SUB_READY, 'in_flight')]);
        await mountAt();
        expect(hasButton(deliveryRow(EVENT_FAILED), 'Send again')).toBe(true);
        expect(buttonIn(deliveryRow(EVENT_FAILED), 'Send again').attributes('disabled')).toBeUndefined();
        for (const id of [EVENT_DELIVERED, EVENT_QUEUED, EVENT_OLDER]) expect(hasButton(deliveryRow(id), 'Send again')).toBe(false);
    });

    it('warns about duplicates, the reset counter, conflicts and lost responses before replaying', async () => {
        deliveries([failed]);
        await mountAt();
        await openReplay();

        const dialog = wrapper!.find('[role="alertdialog"]');
        const text = dialog.text();
        expect(text).toContain(EVENT_FAILED);
        expect(text).toContain(SUB_READY);
        expect(text).toContain(READY_ENDPOINT);
        expect(text).toContain('same event ID');
        expect(text).toContain('an earlier attempt may already have arrived');
        expect(text).toContain('Your receiver should ignore repeats');
        expect(text).toContain('The attempt count starts over');
        expect(text).toContain('automatic retries begin again');
        expect(text).toContain('This only works if the event is still failed');

        // Submitting requires an explicit acknowledgement.
        expect(buttonIn(dialog, 'Send again').attributes('disabled')).toBeDefined();
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.replayWebhookDelivery).not.toHaveBeenCalled();

        await buttonIn(dialog, 'Cancel').trigger('click');
        await flushPromises();
        expect(wrapper!.find('[role="alertdialog"]').exists()).toBe(false);
        expect(api.replayWebhookDelivery).not.toHaveBeenCalled();
    });

    it('replays after confirmation and reloads the history without claiming delivery', async () => {
        deliveries([failed]);
        api.replayWebhookDelivery.mockResolvedValue(replayed);
        await mountAt();
        await openReplay();

        deliveries([{ ...failed, state: 'queued', attempts: 0, next_attempt_at: '2026-11-01T11:00:00Z' }]);
        await acknowledgeAndSubmit();

        expect(api.replayWebhookDelivery).toHaveBeenCalledWith({ path: { appId: APP_ID, eventId: EVENT_FAILED, subscriptionId: SUB_READY } });
        expect(api.replayWebhookDelivery).toHaveBeenCalledTimes(1);
        const toast = vf.showToast.mock.calls[0]?.[0].message as string;
        expect(toast).toContain('Queued to send again');
        expect(toast).toContain('ignore repeats');
        expect(toast).not.toMatch(/\bdelivered\b/i);
        expect(api.listWebhookDeliveries).toHaveBeenCalledTimes(2);
        expect(wrapper!.find('[role="alertdialog"]').exists()).toBe(false);
        expect(deliveryRow(EVENT_FAILED).text()).toContain('Queued');
        expect(hasButton(deliveryRow(EVENT_FAILED), 'Send again')).toBe(false);
    });

    it('explains a 409 and reloads both the history and the endpoints', async () => {
        deliveries([failed]);
        api.replayWebhookDelivery.mockRejectedValue(httpError(409));
        await mountAt();
        await openReplay();
        await acknowledgeAndSubmit();

        const text = history().text();
        expect(text).toContain("The event wasn't sent again, and nothing was queued");
        expect(text).toContain('webhook is disabled or deleted');
        expect(vf.showToast).not.toHaveBeenCalled();
        expect(api.listWebhookDeliveries).toHaveBeenCalledTimes(2);
        expect(api.listAppWebhooks).toHaveBeenCalledTimes(2);
    });

    it.each([
        ['no response', () => networkError(), "Couldn't reach Thinpost"],
        ['a 503', () => httpError(503), 'Sending events again is unavailable right now']
    ])('blocks repeat replays after %s until the history is reloaded', async (_label, error, message) => {
        deliveries([failed]);
        api.replayWebhookDelivery.mockRejectedValue(error());
        await mountAt();
        await openReplay();
        await acknowledgeAndSubmit();

        expect(history().text()).toContain(message);
        expect(history().text()).toContain("can't tell whether the change went through");
        expect(vf.showToast).not.toHaveBeenCalled();
        // No automatic retry and no automatic reload.
        expect(api.replayWebhookDelivery).toHaveBeenCalledTimes(1);
        expect(api.listWebhookDeliveries).toHaveBeenCalledTimes(1);
        expect(buttonIn(deliveryRow(EVENT_FAILED), 'Send again').attributes('disabled')).toBeDefined();
        expect(deliveryRow(EVENT_FAILED).text()).toContain('Reload the history first');

        await buttonIn(history(), 'Refresh').trigger('click');
        await flushPromises();
        expect(api.listWebhookDeliveries).toHaveBeenCalledTimes(2);
        expect(history().text()).not.toContain("can't tell whether the change went through");
        expect(buttonIn(deliveryRow(EVENT_FAILED), 'Send again').attributes('disabled')).toBeUndefined();
    });

    it('treats a success response other than 204 as an uncertain outcome', async () => {
        deliveries([failed]);
        api.replayWebhookDelivery.mockResolvedValue({ data: '<html>', response: new Response('<html>', { status: 200 }) });
        await mountAt();
        await openReplay();
        await acknowledgeAndSubmit();
        expect(history().text()).toContain("didn't understand");
        expect(history().text()).toContain("can't tell whether the change went through");
        expect(vf.showToast).not.toHaveBeenCalled();
        expect(buttonIn(deliveryRow(EVENT_FAILED), 'Send again').attributes('disabled')).toBeDefined();
    });

    it('does not replay when the session ends while the dialog is open', async () => {
        deliveries([failed]);
        await mountAt();
        await openReplay();
        authState.status = 'ended';
        await acknowledgeAndSubmit();
        expect(api.replayWebhookDelivery).not.toHaveBeenCalled();
    });

    it('blocks replay for disabled and deleted endpoints, and for an inactive app', async () => {
        const DELETED = '99999999999999999999999999999999';
        deliveries([delivery(EVENT_FAILED, SUB_DISABLED, 'failed'), delivery(EVENT_QUEUED, DELETED, 'failed')]);
        await mountAt();
        const cases: [string, string, string][] = [
            [EVENT_FAILED, SUB_DISABLED, 'The webhook is disabled.'],
            [EVENT_QUEUED, DELETED, 'This webhook was deleted.']
        ];
        for (const [eventId, subscriptionId, reason] of cases) {
            const row = deliveryRow(eventId, subscriptionId);
            expect(buttonIn(row, 'Send again').attributes('disabled')).toBeDefined();
            expect(row.text()).toContain(reason);
        }
        // An ID missing from the endpoint list is labeled, not shown blank.
        expect(deliveryRow(EVENT_QUEUED, DELETED).text()).toContain('Deleted webhook');
        expect(deliveryRow(EVENT_FAILED, SUB_DISABLED).text()).not.toContain('Deleted webhook');
        expect(history().findAll('th').map(th => th.text())).toContain('Webhook');

        wrapper!.unmount();
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        deliveries([failed]);
        await mountAt();
        expect(buttonIn(deliveryRow(EVENT_FAILED), 'Send again').attributes('disabled')).toBeDefined();
        expect(deliveryRow(EVENT_FAILED).text()).toContain('The app is paused.');
    });

    it('renders cancelled and recovery_hold deliveries instead of an invalid response', async () => {
        deliveries([delivery(EVENT_FAILED, SUB_READY, 'cancelled'), delivery(EVENT_QUEUED, SUB_READY, 'recovery_hold')]);
        await mountAt();
        expect(history().text()).not.toContain("didn't understand");
        expect(deliveryRow(EVENT_FAILED).text()).toContain('Cancelled');
        expect(deliveryRow(EVENT_QUEUED).text()).toContain('On hold');
        expect(hasButton(deliveryRow(EVENT_FAILED), 'Send again')).toBe(false);
        expect(hasButton(deliveryRow(EVENT_QUEUED), 'Send again')).toBe(false);
    });

    it('lists an inbound mail event by its inbound message, and offers Send again when it failed', async () => {
        const INBOUND_ID = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb';
        const inboundFailed = { ...delivery(EVENT_FAILED, SUB_READY, 'failed', { attempts: 12, outcome: 'inbound_received' }), message_id: undefined, inbound_id: INBOUND_ID };
        deliveries([inboundFailed]);
        api.listWebhookDeliveryAttempts.mockResolvedValue({
            data: { attempts: [
                    { state: 'content_unavailable', started_at: '2026-11-01T10:00:00Z', finished_at: '2026-11-01T10:00:01Z' },
                    { state: 'deferred', started_at: '2026-11-01T09:00:00Z', finished_at: '2026-11-01T09:00:01Z' }
                ], truncated: false }
        });
        await mountAt();
        const row = deliveryRow(EVENT_FAILED);
        expect(history().text()).not.toContain("didn't understand");
        expect(row.text()).toContain('inbound_received');
        expect(row.text()).toContain(`Inbound message ${INBOUND_ID}`);
        // The event links to the received message's page.
        expect(row.get('a').attributes('href')).toBe(`/apps/${APP_ID}/inbound/${INBOUND_ID}`);
        expect(hasButton(row, 'Send again')).toBe(true);
        await buttonIn(row, 'Attempts').trigger('click');
        await flushPromises();
        expect(history().find('[role="region"]').text()).toContain('Message gone');
        expect(history().find('[role="region"]').text()).toContain('Postponed');
    });

    it('rejects a delivery with both or neither source', async () => {
        deliveries([{ ...delivery(EVENT_FAILED, SUB_READY, 'failed'), inbound_id: 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' }]);
        await mountAt();
        expect(history().text()).toContain("didn't understand");
        wrapper!.unmount();
        deliveries([{ ...delivery(EVENT_FAILED, SUB_READY, 'failed'), message_id: undefined }]);
        await mountAt();
        expect(history().text()).toContain("didn't understand");
    });

    it('blocks replay while the endpoint list is not loaded', async () => {
        api.listAppWebhooks.mockRejectedValue(networkError());
        deliveries([failed]);
        await mountAt();
        expect(buttonIn(deliveryRow(EVENT_FAILED), 'Send again').attributes('disabled')).toBeDefined();
        expect(deliveryRow(EVENT_FAILED).text()).toContain("webhook list hasn't loaded");
    });
});

describe('webhook delivery refresh', () => {
    it('reloads the endpoint list as well as the history', async () => {
        await mountAt();
        const endpointLoads = api.listAppWebhooks.mock.calls.length;
        await buttonIn(history(), 'Refresh').trigger('click');
        await flushPromises();
        expect(api.listWebhookDeliveries).toHaveBeenCalledTimes(2);
        expect(api.listAppWebhooks).toHaveBeenCalledTimes(endpointLoads + 1);
    });
});
