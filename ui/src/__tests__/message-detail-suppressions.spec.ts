import { flushPromises, type VueWrapper } from '@vue/test-utils';
import type { Router } from 'vue-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    getAppMessage: vi.fn(),
    getAppMessageBody: vi.fn(),
    addAppSuppressions: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { savedSuppressionSearch, clearSavedSuppressionSearches } = await import('@/suppression-search-store');
const { default: MessageDetail } = await import('@/screens/message-detail.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const MESSAGE_ID = 'fedcba9876543210fedcba9876543210';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const summary = {
    id: MESSAGE_ID,
    state: 'prepared',
    status: 'failed',
    enhanced_protection: false,
    subject_masked: false,
    subject: 'Hi',
    sender_domain: 'example.com',
    accepted_at: '2026-10-02T12:00:00Z',
    delivery_count: 3,
    accepted_count: 1,
    failed_count: 2,
    uncertain_count: 0
};
const deliveries = [
    { id: 'a'.repeat(32), recipient_index: 0, recipient: 'ok@example.org', state: 'accepted', retry_attempts: 1, last_completed_outcome: 'accepted', last_smtp_code: 250 },
    { id: 'b'.repeat(32), recipient_index: 1, recipient: 'bounce@example.org', state: 'failed', retry_attempts: 1, last_completed_outcome: 'rejected', last_smtp_code: 550 },
    { id: 'c'.repeat(32), recipient_index: 2, recipient: 'dropped@example.org', state: 'failed', retry_attempts: 0, last_completed_outcome: 'suppressed' }
];

let wrapper: VueWrapper | undefined;
let router: Router;

async function mountAt() {
    const mounted = await mountAppRoute(`/apps/${APP_ID}/messages/${MESSAGE_ID}`, {
        'app-message': MessageDetail,
        'app-suppressions': { template: '<div data-stub="suppressions" />' },
        'app-inbound': { template: '<div data-stub="inbound" />' }
    });
    wrapper = mounted.wrapper;
    router = mounted.router;
}

const row = (n: number) => wrapper!.findAll('tbody tr')[n]!;

async function openMenu(n: number) {
    await row(n).find('.dropdown-trigger[aria-label^="More actions"]').trigger('click');
    await flushPromises();
}

function menuItem(label: string) {
    const item = [...document.querySelectorAll('[role="menuitem"]')].find(el => el.textContent?.includes(label));
    if (!item) throw new Error(`menu item "${label}" not found`);
    return item as HTMLElement;
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    clearSavedSuppressionSearches();
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.getAppMessage.mockResolvedValue({ data: { message: summary, deliveries } });
    api.getAppMessageBody.mockResolvedValue({ data: { from: 'a@example.com', to: [], subject: 'Hi', text: 'x', attachments: [], enhanced_protection: false } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
    document.body.innerHTML = '';
});

describe('message detail suppression actions', () => {
    it('shows a suppressed delivery as Suppressed rather than Failed', async () => {
        await mountAt();
        expect(row(2).find('.tag').text()).toBe('Suppressed');
        expect(row(1).find('.tag').text()).toBe('Failed');
    });

    it('offers no suppression menu for accepted deliveries', async () => {
        await mountAt();
        expect(row(0).find('.dropdown-trigger').exists()).toBe(false);
    });

    it('labels a suppressed last result and finds the address in suppressions without a URL', async () => {
        await mountAt();
        expect(row(2).text()).toContain('Suppressed');
        await openMenu(2);
        expect(document.querySelector('[role="menuitem"]')!.textContent).toContain('Find in suppression list');
        menuItem('Find in suppression list').click();
        await flushPromises();
        expect(router.currentRoute.value.name).toBe('app-suppressions');
        expect(router.currentRoute.value.fullPath).not.toContain('dropped');
        expect(savedSuppressionSearch(APP_ID)).toBe('dropped@example.org');
        expect(api.addAppSuppressions).not.toHaveBeenCalled();
    });

    it('suppresses a failed recipient after confirmation', async () => {
        vf.showConfirm.mockResolvedValue(true);
        api.addAppSuppressions.mockResolvedValue({ data: { results: [{ address: 'bounce@example.org', status: 'added', suppression: {} }] } });
        await mountAt();
        await openMenu(1);
        menuItem('Add to suppression list').click();
        await flushPromises();
        expect(vf.showConfirm).toHaveBeenCalledWith('Add to suppression list?', expect.stringContaining('bounce@example.org'));
        expect(api.addAppSuppressions).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { addresses: ['bounce@example.org'] } });
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Added to suppression list' }));
    });

    it('does not call the API when the confirmation is declined, and shows API errors', async () => {
        vf.showConfirm.mockResolvedValueOnce(false);
        await mountAt();
        await openMenu(1);
        menuItem('Add to suppression list').click();
        await flushPromises();
        expect(api.addAppSuppressions).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(true);
        api.addAppSuppressions.mockRejectedValue(httpError(503));
        await openMenu(1);
        menuItem('Add to suppression list').click();
        await flushPromises();
        expect(wrapper!.text()).toContain('unavailable');
    });
});
