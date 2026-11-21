import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    listAppKeys: vi.fn(),
    issueAppKey: vi.fn(),
    revokeAppKey: vi.fn(),
    getPublicConfig: vi.fn(),
    getSubmissionRate: vi.fn(),
    getRecipientQuota: vi.fn(),
    setRecipientQuota: vi.fn(),
    getPendingQuota: vi.fn(),
    setPendingQuota: vi.fn(),
    getAppSendingIp: vi.fn(),
    listSendingIps: vi.fn(),
    setSubmissionRate: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));
const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showConfirmDestroy: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => ({ ...(await importOriginal<object>()), ...vf }));

const { authState, signOut } = await import('@/auth');
const { default: AppKeys } = await import('@/screens/app-keys.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const KEY_ID = 'fedcba9876543210fedcba9876543210';
const SECRET = 'tp_live_leave_guard_secret';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const activeKey = { id: KEY_ID, name: 'billing-worker', active: true, created_at: '2026-10-02T12:00:00Z' };

let wrapper: VueWrapper | undefined;

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.getSubmissionRate.mockResolvedValue({ data: { per_minute: 600, burst: 100 } });
    api.getRecipientQuota.mockResolvedValue({ data: { daily_limit: 1000, used: 120, day: '2026-11-19' } });
    api.getPendingQuota.mockResolvedValue({ data: { limit: 500, pending: 7, truncated: false } });
    api.getAppSendingIp.mockResolvedValue({ data: { sending_ip_id: null, address: null, helo_name: null } });
    api.listSendingIps.mockResolvedValue({ data: { ips: [], unpinned_app_count: 0, edge_reported_at: null, edge_reports_identity: false, edge_primary_address: null } });
    api.listAppKeys.mockResolvedValue({ data: { keys: [] } });
    api.issueAppKey.mockResolvedValue({ data: { key: activeKey, secret: SECRET } });
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('app keys one-time secret leave guard', () => {
    it('guards sign-out and unload while the secret is shown, and releases on unmount', async () => {
        wrapper = (await mountAppRoute(`/apps/${APP_ID}`, { 'app-keys': AppKeys })).wrapper;
        await wrapper.findAll('button').find(b => b.text().includes('Create key'))!.trigger('click');
        await wrapper.find('input[type="text"]').setValue('billing-worker');
        await wrapper.find('form').trigger('submit');
        await flushPromises();
        expect(wrapper.find<HTMLInputElement>('input[readonly]').element.value).toBe(SECRET);

        const held = new Event('beforeunload', { cancelable: true });
        window.dispatchEvent(held);
        expect(held.defaultPrevented).toBe(true);

        vf.showConfirm.mockResolvedValueOnce(false);
        await signOut();
        expect(vf.showConfirm).toHaveBeenCalledTimes(1);
        expect(authState.status).toBe('signed-in');
        expect(wrapper.find<HTMLInputElement>('input[readonly]').element.value).toBe(SECRET);

        wrapper.unmount();
        wrapper = undefined;
        const released = new Event('beforeunload', { cancelable: true });
        window.dispatchEvent(released);
        expect(released.defaultPrevented).toBe(false);
        await signOut();
        expect(vf.showConfirm).toHaveBeenCalledTimes(1);
        expect(authState.status).toBe('signed-out');
    });
});
