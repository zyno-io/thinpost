import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    getRuntime: vi.fn(),
    getTrackingQuota: vi.fn(),
    setTrackingQuota: vi.fn(),
    listAppDomains: vi.fn(),
    getListUnsubscribe: vi.fn(),
    setListUnsubscribe: vi.fn(),
    getTrackingDefaults: vi.fn(),
    setTrackingDefaults: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showConfirmDestroy: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { default: AppTracking } = await import('@/screens/app-tracking.vue');
const { parseListUnsubscribe } = await import('@/list-unsubscribe');
const { parseRuntimeInfo, parseTrackingDefaults, parseTrackingQuota, readDeliveryTracking, validateDailyLimit } = await import('@/tracking');

const APP_ID = '0123456789abcdef0123456789abcdef';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const enabled = { features: { submission: true, tracking: true }, tracking: { origin: 'https://t.example.com', link_event_limit: 25 } };
const disabled = { features: { submission: true, tracking: false } };
const quota = { daily_limit: 500, used: 120, suppressed: 7, day: '2026-11-11' };

let wrapper: VueWrapper | undefined;

async function mountTab() {
    const mounted = await mountAppRoute(`/apps/${APP_ID}/tracking`, { 'app-tracking': AppTracking });
    wrapper = mounted.wrapper;
}
const dialog = () => wrapper!.find('[role="dialog"]');
function button(label: string) {
    const match = wrapper!.findAll('button').find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}
async function openEditor() {
    await button('Edit').trigger('click');
    await flushPromises();
    const input = dialog().find('input[type="text"]');
    await vi.waitFor(() => expect(document.activeElement).toBe(input.element));
    return input;
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    api.listApps.mockResolvedValue({ data: { apps } as never });
    api.getRuntime.mockResolvedValue({ data: enabled });
    api.getTrackingQuota.mockResolvedValue({ data: quota });
    api.getListUnsubscribe.mockResolvedValue({ data: { enabled: false } });
    api.getTrackingDefaults.mockResolvedValue({ data: { opens: false, clicks: false } });
});
const apps = [app];

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
    document.body.innerHTML = '';
});

describe('tracking rules', () => {
    it('validates the daily limit like the server', () => {
        expect(validateDailyLimit('0')).toEqual({ value: 0, error: null });
        expect(validateDailyLimit('1000000').value).toBe(1_000_000);
        expect(validateDailyLimit('1000001').error).toBe('Enter at most 1,000,000.');
        expect(validateDailyLimit('').error).toContain('Enter a daily limit');
        for (const raw of ['-1', '1.5', '1,000', 'x']) expect(validateDailyLimit(raw).error).toContain('whole number');
    });

    it('reads the recipient policy only when it is one of the two known values', () => {
        expect(parseRuntimeInfo({ features: { submission: true, tracking: false, recipient_policy: 'allowlist' } }).features.recipient_policy).toBe('allowlist');
        expect(parseRuntimeInfo({ features: { submission: true, tracking: false, recipient_policy: 'unrestricted' } }).features.recipient_policy).toBe('unrestricted');
        for (const value of [undefined, 'other', 1, null]) {
            expect(parseRuntimeInfo({ features: { submission: true, tracking: false, recipient_policy: value } }).features.recipient_policy).toBeUndefined();
        }
    });

    it('accepts only contract-valid responses', () => {
        expect(parseRuntimeInfo(disabled).tracking).toBeUndefined();
        // A server that predates custom tracking domains omits the new fields: no custom domains.
        expect(parseRuntimeInfo(enabled).tracking).toEqual({ origin: 'https://t.example.com', link_event_limit: 25, custom_domains: false, cname_target: '' });
        const custom = { ...enabled, tracking: { ...enabled.tracking, custom_domains: true, cname_target: 'links.example.net' } };
        expect(parseRuntimeInfo(custom).tracking).toEqual({ origin: 'https://t.example.com', link_event_limit: 25, custom_domains: true, cname_target: 'links.example.net' });
        // Only a literal true turns custom domains on, and a malformed target is dropped.
        expect(parseRuntimeInfo({ ...enabled, tracking: { ...enabled.tracking, custom_domains: 'yes', cname_target: 5 } }).tracking).toMatchObject({ custom_domains: false, cname_target: '' });
        for (const body of [null, '<html>', {}, { features: { submission: true } }, { ...enabled, tracking: { origin: '', link_event_limit: 5 } }, { ...enabled, tracking: { origin: 'x', link_event_limit: 0 } }]) {
            expect(() => parseRuntimeInfo(body)).toThrow("something the console didn't understand");
        }
        expect(parseTrackingQuota(quota)).toEqual(quota);
        for (const body of [null, {}, { ...quota, used: -1 }, { ...quota, day: 'today' }, { ...quota, daily_limit: 1_000_001 }]) {
            expect(() => parseTrackingQuota(body)).toThrow("something the console didn't understand");
        }
    });

    it('treats malformed engagement as not reported', () => {
        expect(readDeliveryTracking({})).toBeNull();
        expect(readDeliveryTracking({ tracking: { opens: -1, clicks: 0 } })).toBeNull();
        expect(readDeliveryTracking({ tracking: { opens: 2, clicks: 0 } })).toEqual({ opens: 2, clicks: 0 });
    });
});

describe('tracking tab', () => {
    it('shows the deployment and the budget with their sources', async () => {
        await mountTab();
        const text = wrapper!.text();
        expect(text).toContain('Available');
        expect(text).toContain('https://t.example.com');
        expect(text).toContain('As of');
        const facts = wrapper!.find('[data-testid="quota-facts"]').text();
        expect(facts).toContain('Limit500');
        expect(facts).toContain('Recorded today120');
        expect(facts).toContain('Over limit7');
        expect(facts).toContain('2026-11-11');
        expect(await openHelp(wrapper!, 'daily tracking limit')).toContain('Tracked links always work and redirect');
    });

    describe('own tracking address summary', () => {
        const withCustom = { ...enabled, tracking: { ...enabled.tracking, custom_domains: true, cname_target: 'links.example.net' } };
        const domain = (name: string, patch: object = {}) => ({ name, active: true, created_at: '2026-10-02T12:00:00Z', ...patch });
        const summary = () => wrapper!.find('[data-testid="tracking-domains-summary"]');

        it('says it is not turned on when the installation does not offer it, and does not load domains', async () => {
            await mountTab();
            expect(summary().text()).toContain("isn't turned on for this Thinpost installation");
            expect(api.listAppDomains).not.toHaveBeenCalled();
        });

        it('is hidden when tracking itself is off', async () => {
            api.getRuntime.mockResolvedValue({ data: disabled });
            await mountTab();
            expect(summary().exists()).toBe(false);
        });

        it('lists each verified domain read-only, with where its tracking address stands', async () => {
            api.getRuntime.mockResolvedValue({ data: withCustom });
            api.listAppDomains.mockResolvedValue({
                data: {
                    domains: [
                        domain('a.example.com', { tracking_domain: { name: 'links.a.example.com', state: 'active', checking: false, https_result: 'ok', created_at: 'x' } }),
                        domain('b.example.com', { tracking_domain: { name: 'links.b.example.com', state: 'pending', checking: false, created_at: 'x' } }),
                        domain('c.example.com'),
                        domain('d.example.com', { active: false })
                    ]
                }
            });
            await mountTab();
            expect(api.listAppDomains).toHaveBeenCalledWith({ path: { appId: APP_ID } });
            expect(summary().find('[data-testid="tracking-summary-a.example.com"]').text()).toContain('links.a.example.com');
            expect(summary().find('[data-testid="tracking-summary-a.example.com"]').text()).toContain('In use');
            expect(summary().find('[data-testid="tracking-summary-b.example.com"]').text()).toContain('Waiting for setup');
            expect(summary().find('[data-testid="tracking-summary-c.example.com"]').text()).toContain('Standard tracking address');
            expect(summary().find('[data-testid="tracking-summary-d.example.com"]').exists()).toBe(false);
            // Read-only: it links to the Sending domains tab and has no editing controls.
            expect(summary().findAll('button').filter(b => /Add|Change|Remove|Check/.test(b.text()))).toHaveLength(0);
            expect(summary().find('a').text()).toContain('Manage on Sending domains');
        });

        it('shows a load failure as unavailable, not as no domains', async () => {
            api.getRuntime.mockResolvedValue({ data: withCustom });
            api.listAppDomains.mockRejectedValue(httpError(503));
            await mountTab();
            expect(summary().text()).toContain("couldn't be loaded right now");
            expect(summary().text()).not.toContain('No sending domain has its own tracking address');
        });
    });

    it('says what disabled tracking means and invents nothing', async () => {
        api.getRuntime.mockResolvedValue({ data: disabled });
        await mountTab();
        expect(wrapper!.text()).toContain('Not available');
        expect(wrapper!.text()).toContain('sends that ask for tracking are rejected, app defaults have no effect, and links go out unchanged.');
        expect(wrapper!.find('[data-testid="tracking-facts"]').exists()).toBe(false);
        expect(wrapper!.text()).not.toMatch(/unique|open rate|click rate/i);
    });

    it('shows unavailable data as unavailable, not as zeros', async () => {
        api.getRuntime.mockRejectedValue(httpError(503));
        api.getTrackingQuota.mockRejectedValue(httpError(503));
        await mountTab();
        expect(wrapper!.text()).toContain("Tracking availability couldn't be loaded right now");
        expect(wrapper!.text()).toContain('The daily tracking limit is temporarily unavailable');
        expect(wrapper!.find('[data-testid="quota-facts"]').exists()).toBe(false);
        expect(button('Edit').attributes('disabled')).toBeDefined();
    });

    it('saves a new limit only after confirmation and shows the result', async () => {
        api.setTrackingQuota.mockResolvedValue({ data: { ...quota, daily_limit: 900 } });
        await mountTab();
        const input = await openEditor();
        await input.setValue('900');
        const submit = dialog().find('button[type="submit"]');
        expect(submit.attributes('disabled')).toBeDefined();
        await dialog().find('input[type="checkbox"]').setValue(true);
        await dialog().find('form').trigger('submit');
        await flushPromises();
        expect(api.setTrackingQuota).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { daily_limit: 900 } });
        expect(dialog().exists()).toBe(false);
        expect(wrapper!.find('[data-testid="quota-facts"]').text()).toContain('Limit900');
        expect(vf.showToast).toHaveBeenCalled();
    });

    it('rejects an out-of-range limit client-side and warns about zero', async () => {
        await mountTab();
        const input = await openEditor();
        await input.setValue('1000001');
        await input.trigger('blur');
        expect(dialog().text()).toContain('Enter at most 1,000,000.');
        await input.setValue('0');
        expect(dialog().text()).toContain('No opens or clicks will be recorded');
        expect(api.setTrackingQuota).not.toHaveBeenCalled();
    });

    it('marks the limit possibly stale after an uncertain outcome', async () => {
        api.setTrackingQuota.mockRejectedValue(httpError(502));
        await mountTab();
        const input = await openEditor();
        await input.setValue('900');
        await dialog().find('input[type="checkbox"]').setValue(true);
        await dialog().find('form').trigger('submit');
        await flushPromises();
        expect(dialog().text()).toContain("We can't tell whether the change went through");
        await button('Cancel').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain('may or may not have gone through');
    });
});

describe('one-click unsubscribe panel', () => {
    const toggle = () => wrapper!.find('[data-testid="list-unsubscribe-toggle"]');

    it('is off by default and explains the transactional versus bulk choice', async () => {
        await mountTab();
        expect(api.getListUnsubscribe).toHaveBeenCalledWith({ path: { appId: APP_ID } });
        expect(wrapper!.text()).toContain('One-click unsubscribe');
        expect(toggle().text()).toContain('Turn on');
        const help = await openHelp(wrapper!, 'one-click unsubscribe');
        expect(help).toContain('transactional');
        expect(help).toContain('tracking');
    });

    it('turns on only after confirmation', async () => {
        api.setListUnsubscribe.mockResolvedValue({ data: { enabled: true } });
        vf.showConfirm.mockResolvedValue(false);
        await mountTab();
        await toggle().trigger('click');
        await flushPromises();
        expect(api.setListUnsubscribe).not.toHaveBeenCalled();
        vf.showConfirm.mockResolvedValue(true);
        await toggle().trigger('click');
        await flushPromises();
        expect(api.setListUnsubscribe).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { enabled: true } });
        expect(toggle().text()).toContain('Turn off');
        expect(wrapper!.text()).toContain('Saved: on.');
    });

    it('turns off without a confirmation', async () => {
        api.getListUnsubscribe.mockResolvedValue({ data: { enabled: true } });
        api.setListUnsubscribe.mockResolvedValue({ data: { enabled: false } });
        await mountTab();
        await toggle().trigger('click');
        await flushPromises();
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(api.setListUnsubscribe).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { enabled: false } });
        expect(toggle().text()).toContain('Turn on');
    });

    it('is disabled with the reason when tracking is not configured, but can still be turned off', async () => {
        api.getRuntime.mockResolvedValue({ data: disabled });
        await mountTab();
        expect(toggle().attributes('disabled')).toBeDefined();
        expect(wrapper!.find('[data-testid="list-unsubscribe-blocker"]').text()).toContain("tracking isn't available for this Thinpost installation");
        wrapper!.unmount();
        api.getListUnsubscribe.mockResolvedValue({ data: { enabled: true } });
        await mountTab();
        expect(toggle().attributes('disabled')).toBeUndefined();
    });

    it('reports a 409 from the server as nothing changed', async () => {
        api.setListUnsubscribe.mockRejectedValue(httpError(409));
        vf.showConfirm.mockResolvedValue(true);
        await mountTab();
        await toggle().trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("because tracking isn't available for this Thinpost installation, or its tracking address isn't usable");
        expect(toggle().text()).toContain('Turn on');
    });

    it('shows unavailable data as unavailable', async () => {
        api.getListUnsubscribe.mockRejectedValue(httpError(503));
        await mountTab();
        expect(wrapper!.text()).toContain('One-click unsubscribe is temporarily unavailable');
        expect(toggle().attributes('disabled')).toBeDefined();
    });

    it('accepts only a contract-valid response', () => {
        expect(parseListUnsubscribe({ enabled: true })).toEqual({ enabled: true });
        for (const body of [null, {}, { enabled: 'yes' }, '<html>']) expect(() => parseListUnsubscribe(body)).toThrow("something the console didn't understand");
    });
});

describe('tracking defaults panel', () => {
    const toggle = (kind: 'opens' | 'clicks') => wrapper!.find(`[data-testid="tracking-defaults-${kind}-toggle"]`);
    const facts = () => wrapper!.find('[data-testid="tracking-defaults-facts"]').text();

    it('shows both kinds off by default and explains the rules', async () => {
        await mountTab();
        expect(api.getTrackingDefaults).toHaveBeenCalledWith({ path: { appId: APP_ID } });
        expect(wrapper!.text()).toContain('Tracking defaults');
        expect(facts()).toContain('OpensOffTurn on');
        expect(facts()).toContain('ClicksOffTurn on');
        const help = await openHelp(wrapper!, 'tracking defaults');
        expect(help).toContain('always wins');
        expect(help).toContain('TrackOpens');
        expect(help).toContain('A default never causes a message to be rejected');
        expect(help).toContain("Defaults have no effect while tracking isn't available for this Thinpost installation");
    });

    it('turns a default on only after confirmation and sends both values', async () => {
        api.setTrackingDefaults.mockResolvedValue({ data: { opens: false, clicks: true } });
        vf.showConfirm.mockResolvedValue(false);
        await mountTab();
        await toggle('clicks').trigger('click');
        await flushPromises();
        expect(api.setTrackingDefaults).not.toHaveBeenCalled();
        expect(vf.showConfirm.mock.calls[0]?.[1]).toContain('password-reset and sign-in links');
        vf.showConfirm.mockResolvedValue(true);
        await toggle('clicks').trigger('click');
        await flushPromises();
        expect(api.setTrackingDefaults).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { opens: false, clicks: true } });
        expect(facts()).toContain('ClicksOnTurn off');
        expect(wrapper!.text()).toContain('Saved: opens off, clicks on.');
    });

    it('turns off without confirmation, carrying the other kind as loaded', async () => {
        api.getTrackingDefaults.mockResolvedValue({ data: { opens: true, clicks: true } });
        api.setTrackingDefaults.mockResolvedValue({ data: { opens: true, clicks: false } });
        await mountTab();
        await toggle('clicks').trigger('click');
        await flushPromises();
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(api.setTrackingDefaults).toHaveBeenCalledWith({ path: { appId: APP_ID }, body: { opens: true, clicks: false } });
        expect(facts()).toContain('OpensOnTurn off');
        expect(facts()).toContain('ClicksOffTurn on');
    });

    it('blocks turning on when tracking is not configured, flags a stored default as not applied, and still allows turning off', async () => {
        api.getRuntime.mockResolvedValue({ data: disabled });
        api.getTrackingDefaults.mockResolvedValue({ data: { opens: false, clicks: true } });
        await mountTab();
        expect(toggle('opens').attributes('disabled')).toBeDefined();
        expect(toggle('clicks').attributes('disabled')).toBeUndefined();
        expect(wrapper!.find('[data-testid="tracking-defaults-blocker"]').text()).toContain('these defaults have no effect');
        expect(wrapper!.find('[data-testid="tracking-defaults-clicks-inert"]').text()).toBe('No effect');
        expect(wrapper!.find('[data-testid="tracking-defaults-opens-inert"]').exists()).toBe(false);
    });

    it('does not block on the client while the deployment status is unknown', async () => {
        api.getRuntime.mockRejectedValue(httpError(503));
        await mountTab();
        expect(toggle('opens').attributes('disabled')).toBeUndefined();
        expect(wrapper!.find('[data-testid="tracking-defaults-blocker"]').exists()).toBe(false);
    });

    it('reports a 409 from the server as nothing changed', async () => {
        api.setTrackingDefaults.mockRejectedValue(httpError(409));
        vf.showConfirm.mockResolvedValue(true);
        await mountTab();
        await toggle('opens').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("so a default can't be turned on. Ask your Thinpost administrator to enable tracking. Nothing was changed.");
        expect(facts()).toContain('OpensOffTurn on');
    });

    it('shows unavailable data as unavailable and disables the toggles', async () => {
        api.getTrackingDefaults.mockRejectedValue(httpError(503));
        await mountTab();
        expect(wrapper!.text()).toContain('Tracking defaults are temporarily unavailable');
        expect(wrapper!.find('[data-testid="tracking-defaults-facts"]').exists()).toBe(false);
        expect(toggle('opens').exists()).toBe(false);
    });

    it('accepts only a contract-valid response', () => {
        expect(parseTrackingDefaults({ opens: true, clicks: false })).toEqual({ opens: true, clicks: false });
        for (const body of [null, {}, { opens: true }, { opens: 'yes', clicks: false }, '<html>']) expect(() => parseTrackingDefaults(body)).toThrow("something the console didn't understand");
    });
});
