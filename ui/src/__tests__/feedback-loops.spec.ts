import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import { installVf } from '@zyno-io/vue-foundation';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, openHelp } from './helpers';

const api = vi.hoisted(() => ({
    getFeedbackLoops: vi.fn(),
    createFeedbackMailbox: vi.fn(),
    setFeedbackReporters: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showConfirmDestroy: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { default: Card } = await import('@/shared/components/feedback-loops-card.vue');
const { countRows, parseFeedbackLoops, parseReporterDomains } = await import('@/feedback-loops');
const { WEBHOOK_FORMATS } = await import('@/webhook-events');

const ADDRESS = 'b+0123456789abcdef0123456789abcdef@mx.example.net';
const today = new Date().toISOString().slice(0, 10);

function state(overrides: Record<string, unknown> = {}) {
    return {
        receiving: true,
        mailbox: { address: ADDRESS, created_at: '2026-11-10T10:00:00Z' },
        reporters: ['arf.mail.yahoo.com'],
        counts: [
            { day: today, channel: 'arf', result: 'accepted', count: 2 },
            { day: '2026-01-01', channel: 'arf', result: 'accepted', count: 5 },
            { day: today, channel: 'arf', result: 'unauthenticated', count: 7 }
        ],
        ...overrides
    };
}

let wrapper: VueWrapper | undefined;
async function mountCard(gateMode: string | null = 'open') {
    wrapper = mount(Card, { props: { gateMode }, global: { plugins: [installVf] }, attachTo: document.body });
    await flushPromises();
}
function button(label: string) {
    const match = wrapper!.findAll('button').find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    api.getFeedbackLoops.mockResolvedValue({ data: state() });
});
afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
    document.body.innerHTML = '';
});

describe('feedback loop helpers', () => {
    it('parses reporter domains like the server and reports the first problem', () => {
        expect(parseReporterDomains(' ARF.Mail.Yahoo.com\nb.example,  b.example ')).toEqual({ domains: ['arf.mail.yahoo.com', 'b.example'], problem: null });
        expect(parseReporterDomains('')).toEqual({ domains: [], problem: null });
        for (const bad of ['localhost', 'a b.example x_y', '10.0.0.1', 'bücher.example', '-a.example', 'a..example']) {
            expect(parseReporterDomains(bad).problem, bad).toBeTruthy();
        }
        expect(parseReporterDomains(Array.from({ length: 33 }, (_, i) => `d${i}.example`).join('\n')).problem).toContain('At most 32');
    });

    it('rejects a malformed response', () => {
        expect(parseFeedbackLoops(state())).toBeTruthy();
        for (const body of [null, '<html>', {}, state({ reporters: 'x' }), state({ counts: [{ day: 'x', channel: 'arf', result: 'accepted', count: 1 }] }), state({ counts: [{ day: today, channel: 'arf', result: 'bogus', count: 1 }] }), state({ mailbox: { created_at: 'x' } })]) {
            expect(() => parseFeedbackLoops(body)).toThrow("didn't understand");
        }
    });

    it('sums counts per result', () => {
        const rows = countRows(state().counts as never, new Date());
        expect(rows.find(r => r.result === 'accepted')).toEqual({ result: 'accepted', today: 2, total: 7 });
        expect(rows.find(r => r.result === 'duplicate')).toEqual({ result: 'duplicate', today: 0, total: 0 });
    });

    it('offers the complained event for every format that can represent it', () => {
        expect(WEBHOOK_FORMATS.thinpost.groups.flatMap(g => g.events)).toContain('complained');
        expect(WEBHOOK_FORMATS.postmark.groups.flatMap(g => g.events)).toContain('SpamComplaint');
        expect(WEBHOOK_FORMATS.resend.groups.flatMap(g => g.events)).toContain('email.complained');
        expect(WEBHOOK_FORMATS.mailgun.groups.flatMap(g => g.events)).toContain('complained');
    });
});

describe('feedback loops card', () => {
    it('shows the mailbox address with copy, the reporters and the 14-day counts', async () => {
        await mountCard();
        expect((wrapper!.find('[data-testid="mailbox-address"] input').element as HTMLInputElement).value).toBe(ADDRESS);
        expect(wrapper!.text()).toContain('arf.mail.yahoo.com');
        expect(wrapper!.text()).toContain('Receiving');
        const accepted = wrapper!.find('[data-result="accepted"]').findAll('td').map(c => c.text());
        expect(accepted).toEqual(['2', '7']);
        expect(wrapper!.find('[data-result="duplicate"]').findAll('td').map(c => c.text())).toEqual(['0', '0']);
    });

    it('says plainly that nothing is accepted without reporters, and when there is no mailbox', async () => {
        api.getFeedbackLoops.mockResolvedValue({ data: state({ reporters: [] }) });
        await mountCard();
        expect(wrapper!.find('[data-testid="no-reporters"]').text()).toBe('Accepting nothing');
        expect(wrapper!.find('[data-testid="reporters-empty"]').text()).toContain('no reports are accepted');
        wrapper!.unmount();
        api.getFeedbackLoops.mockResolvedValue({ data: state({ mailbox: undefined }) });
        await mountCard();
        expect(wrapper!.text()).toContain('No mailbox');
        expect(wrapper!.find('[data-testid="create-mailbox"]').exists()).toBe(true);
        expect(wrapper!.find('[data-testid="rotate"]').exists()).toBe(false);
    });

    it('represents a replica without a bounce domain instead of inventing an address', async () => {
        api.getFeedbackLoops.mockResolvedValue({ data: state({ receiving: false, mailbox: { created_at: '2026-11-10T10:00:00Z' } }) });
        await mountCard();
        expect(wrapper!.find('[data-testid="mailbox-address"]').exists()).toBe(false);
        expect(wrapper!.find('[data-testid="no-address"]').exists()).toBe(true);
    });

    it('shows a load failure with a retry and no stale data', async () => {
        api.getFeedbackLoops.mockRejectedValueOnce(httpError(503));
        await mountCard();
        expect(wrapper!.text()).toContain('unavailable');
        expect(wrapper!.find('[data-testid="feedback-counts"]').exists()).toBe(false);
        await button('Reload').trigger('click');
        await flushPromises();
        expect(wrapper!.find('[data-testid="feedback-counts"]').exists()).toBe(true);
    });

    it('creates the mailbox with an empty body and reloads', async () => {
        api.getFeedbackLoops.mockResolvedValueOnce({ data: state({ mailbox: undefined }) });
        api.createFeedbackMailbox.mockResolvedValue({ data: { address: ADDRESS } });
        await mountCard();
        await wrapper!.find('[data-testid="create-mailbox"]').trigger('click');
        await flushPromises();
        expect(api.createFeedbackMailbox).toHaveBeenCalledWith({ body: {} });
        expect(api.getFeedbackLoops).toHaveBeenCalledTimes(2);
    });

    it('rotates only after the confirmation that every loop must be re-registered', async () => {
        api.createFeedbackMailbox.mockResolvedValue({ data: { address: ADDRESS } });
        await mountCard();
        vf.showConfirmDestroy.mockResolvedValueOnce(false);
        await wrapper!.find('[data-testid="rotate"]').trigger('click');
        await flushPromises();
        expect(vf.showConfirmDestroy.mock.calls[0]?.[1]).toContain('register the new address with every feedback loop');
        expect(api.createFeedbackMailbox).not.toHaveBeenCalled();
        vf.showConfirmDestroy.mockResolvedValueOnce(true);
        await wrapper!.find('[data-testid="rotate"]').trigger('click');
        await flushPromises();
        expect(api.createFeedbackMailbox).toHaveBeenCalledWith({ body: { rotate: true } });
    });

    it('edits the reporter domains through the modal', async () => {
        api.setFeedbackReporters.mockResolvedValue({ data: { domains: [] } });
        await mountCard();
        await wrapper!.find('[data-testid="edit-reporters"]').trigger('click');
        await flushPromises();
        const textarea = document.querySelector('textarea') as HTMLTextAreaElement;
        expect(textarea.value).toBe('arf.mail.yahoo.com');
        textarea.value = 'arf.mail.yahoo.com\nReports.Example.NET';
        textarea.dispatchEvent(new Event('input'));
        await flushPromises();
        (document.querySelector('button[type="submit"]') as HTMLButtonElement).click();
        await flushPromises();
        expect(api.setFeedbackReporters).toHaveBeenCalledWith({ body: { domains: ['arf.mail.yahoo.com', 'reports.example.net'] } });
    });

    it('blocks changes while the gate is closed or the session ended', async () => {
        await mountCard('closed');
        expect(button('Replace address').attributes('disabled')).toBeDefined();
        expect(wrapper!.find('[data-testid="edit-reporters"]').attributes('disabled')).toBeDefined();
        wrapper!.unmount();
        authState.status = 'signed-out' as never;
        await mountCard('open');
        expect(button('Replace address').attributes('disabled')).toBeDefined();
    });

    it('explains the setup behind the ? button', async () => {
        await mountCard();
        const text = await openHelp(wrapper!, 'feedback loops');
        expect(text).toContain('arf.mail.yahoo.com');
        expect(text).toContain('SNDS');
        expect(text).toContain('Postmaster Tools');
    });
});
