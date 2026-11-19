import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, networkError, openHelp } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    getAppMessage: vi.fn(),
    getAppMessageBody: vi.fn(),
    downloadAppMessageAttachment: vi.fn(),
    getAppDeliveryHeaders: vi.fn(),
    listAppMessages: vi.fn(),
    searchAppMessages: vi.fn(),
    retryDeliveryPolicyHold: vi.fn(),
    abandonDeliveryTrackingHold: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { openModalCount } = await import('@/shared/modal-state');
const { clearSavedSearches } = await import('@/message-search-store');
const { default: MessageDetail } = await import('@/screens/message-detail.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const MESSAGE_ID = 'fedcba9876543210fedcba9876543210';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };
const summary = {
    id: MESSAGE_ID,
    state: 'prepared',
    status: 'attention',
    enhanced_protection: true,
    subject_masked: true,
    sender_domain: 'example.com',
    accepted_at: '2026-10-02T12:00:00Z',
    delivery_count: 2,
    accepted_count: 1,
    failed_count: 0,
    uncertain_count: 1
};
const plain = { ...summary, enhanced_protection: false, subject_masked: false, subject: 'LEAK' };
const deliveries = [
    { id: 'a'.repeat(32), recipient_index: 0, state: 'accepted' },
    { id: 'b'.repeat(32), recipient_index: 1, state: 'uncertain' }
];
const HTML = '<img src=x onerror="window.__pwned=1"><script>window.__pwned=2</script><b>Hi</b>';
const body = {
    from: 'billing@example.com',
    to: ['ada@example.org', 'grace@example.net'],
    subject: 'Your invoice <b>',
    text: 'Hello\nworld',
    html: HTML,
    enhanced_protection: true,
    attachments: []
};

let wrapper: VueWrapper | undefined;

async function mountAt(path: string) {
    const mounted = await mountAppRoute(path, { 'app-message': MessageDetail });
    wrapper = mounted.wrapper;
    return wrapper;
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
    delete (window as { __pwned?: unknown }).__pwned;
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.getAppMessage.mockResolvedValue({ data: { message: summary, deliveries } });
    api.getAppMessageBody.mockResolvedValue({ data: body });
    clearSavedSearches();
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('message detail screen', () => {
    it('does not call the API for malformed IDs', async () => {
        await mountAt(`/apps/${APP_ID}/messages/not-an-id`);
        expect(wrapper!.text()).toContain('Message not found');
        expect(api.getAppMessage).not.toHaveBeenCalled();
    });

    it('shows metadata and deliveries for an Enhanced Protection message without fetching its content', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(api.getAppMessage).toHaveBeenCalledWith({ path: { appId: APP_ID, messageId: MESSAGE_ID } });
        expect(api.getAppMessageBody).not.toHaveBeenCalled();
        const text = wrapper!.text();
        expect(text).not.toContain("Accepted means the recipient's mail server took the message. It doesn't guarantee it reached the inbox");
        expect(await openHelp(wrapper!, 'deliveries')).toContain("Accepted means the recipient's mail server took the message. It doesn't guarantee it reached the inbox");
        expect(wrapper!.findAll('tbody tr')).toHaveLength(2);
        expect(text).toContain('Uncertain');
        expect(wrapper!.find('#message-heading').element.parentElement!.querySelector('.tag')!.textContent).toContain('Needs attention');
        expect(text).not.toContain('ada@example.org');
        expect(text).not.toContain('LEAK');
    });

    it.each([true, false])('explains Enhanced Protection and what the TLS limits mean (enhanced_protection %s)', async enhanced => {
        api.getAppMessage.mockResolvedValue({
            data: { message: { ...summary, enhanced_protection: enhanced, subject_masked: enhanced, subject: 'LEAK' }, deliveries }
        });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const fact = wrapper!.find('[data-fact="enhanced-protection"]');
        expect(fact.find('dd').text()).toBe(enhanced ? 'On' : 'Off');

        // Compact indicator on the page; the explanation is behind the help button.
        expect(fact.text()).not.toContain('STARTTLS');
        expect(wrapper!.find('[data-testid="ep-hint"]').exists()).toBe(false);
        if (!enhanced) expect(wrapper!.text()).not.toMatch(/audit/i);
        const text = await openHelp(wrapper!, 'Enhanced Protection');
        expect(text).toContain("encrypted (TLS) connection whose certificate matches the mail server's name");
        expect(text).toContain("That name comes from DNS lookups that aren't authenticated");
        expect(text).toContain("not that it is the recipient's intended mail server");
        expect(text).toContain('It protects only the connection to that mail server');
        expect(text).toContain('Later hops may not use TLS');
        expect(text).toContain('this is not end-to-end encryption');
        expect(text).toContain("doesn't by itself make you HIPAA compliant");
        expect(text).toContain("doesn't show the encryption result of individual delivery attempts");
        expect(text).not.toMatch(/strict MX|MX policy/i);
        expect(text).not.toMatch(/authenticated DNS|DNSSEC|end-to-end encrypted|guarantee|ensures|(?<!make you )HIPAA[- ]compliant/i);
        // Must not imply Thinpost enforces TLS beyond the first hop.
        expect(text).not.toMatch(/each hop|every hop|all hops/i);

        const dialogText = document.querySelectorAll('[role="dialog"]');
        const audit = dialogText[dialogText.length - 1]!.querySelector('[data-testid="ep-audit-hint"]');
        if (enhanced) {
            expect(audit?.textContent).toContain('audit log');
            expect(wrapper!.find('[data-fact="subject"]').text()).toContain('Protected. Shown with the message content.');
            expect(wrapper!.text()).not.toContain('LEAK');
        } else {
            expect(audit).toBeNull();
            expect(text).not.toMatch(/audit/i);
            expect(wrapper!.find('[data-fact="subject"]').text()).toContain('LEAK');
        }
    });

    it('auto-loads non-EP content without a dialog', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(api.getAppMessageBody).toHaveBeenCalledTimes(1);
        expect(api.getAppMessageBody).toHaveBeenCalledWith({ path: { appId: APP_ID, messageId: MESSAGE_ID } });
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('ada@example.org');
        expect(wrapper!.text()).not.toMatch(/audit/i);
    });

    it('does not fetch EP content until confirmed', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(api.getAppMessageBody).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(false);
        await button('Show content').trigger('click');
        await flushPromises();
        expect(vf.showConfirm).toHaveBeenCalledWith(expect.any(String), expect.stringContaining('audit log'));
        expect(api.getAppMessageBody).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(true);
        await button('Show content').trigger('click');
        await flushPromises();
        expect(api.getAppMessageBody).toHaveBeenCalledWith({ path: { appId: APP_ID, messageId: MESSAGE_ID } });
        expect(wrapper!.text()).toContain('ada@example.org');
        expect(wrapper!.text()).toContain('Your invoice <b>');
    });

    it('renders HTML only in a sandboxed iframe and shows the source as inert text', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);

        expect(document.querySelectorAll('#message-detail iframe[sandbox=""]')).toHaveLength(1);
        expect(document.querySelector('#message-detail img, #message-detail script, #message-detail b')).toBeNull();
        expect((window as { __pwned?: unknown }).__pwned).toBeUndefined();

        await wrapper!.findAll('[role=tab]').find(t => t.text() === 'HTML source')!.trigger('click');
        const pre = wrapper!.find('pre[aria-label="HTML part source"]');
        expect(pre.text()).toBe(HTML);
        expect(pre.element.children).toHaveLength(0);
        await wrapper!.findAll('[role=tab]').find(t => t.text() === 'Text')!.trigger('click');
        expect(wrapper!.find('pre[aria-label="Text part"]').text()).toBe('Hello\nworld');
    });

    it('discards non-EP content when hidden and when the session ends, and never stores it', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        const setItem = vi.spyOn(Storage.prototype, 'setItem');
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(document.body.innerHTML).toContain('ada@example.org');

        await button('Hide content').trigger('click');
        expect(document.body.innerHTML).not.toContain('ada@example.org');

        await button('Show content').trigger('click');
        await flushPromises();
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(document.body.innerHTML).toContain('ada@example.org');
        authState.status = 'ended';
        await flushPromises();
        expect(document.body.innerHTML).not.toContain('ada@example.org');
        expect(button('Show content').attributes('disabled')).toBeDefined();

        expect(setItem.mock.calls.flat().join(' ')).not.toContain('ada@example.org');
        setItem.mockRestore();
    });

    it('drops an auto-load response that arrives after the session ended', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        let resolveBody: (value: unknown) => void = () => undefined;
        api.getAppMessageBody.mockReturnValue(new Promise(resolve => (resolveBody = resolve)));
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);

        authState.status = 'ended';
        await flushPromises();
        resolveBody({ data: body });
        await flushPromises();
        expect(document.body.innerHTML).not.toContain('ada@example.org');
    });

    it('does not fetch EP content when the user confirms after leaving the page', async () => {
        let confirm: (ok: boolean) => void = () => undefined;
        vf.showConfirm.mockReturnValue(new Promise(resolve => (confirm = resolve)));
        const mounted = await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const router = mounted.vm.$router;
        await button('Show content').trigger('click');
        await flushPromises();

        await router.push({ name: 'app-messages', params: { appId: APP_ID } });
        await flushPromises();
        confirm(true);
        await flushPromises();
        expect(api.getAppMessageBody).not.toHaveBeenCalled();
    });

    it('does not reload or discard non-EP content on a hash-only navigation', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        const mounted = await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const router = mounted.vm.$router;

        await router.push({ hash: '#main-content' });
        await flushPromises();
        expect(api.getAppMessage).toHaveBeenCalledTimes(1);
        expect(api.getAppMessageBody).toHaveBeenCalledTimes(1);
        expect(document.body.innerHTML).toContain('ada@example.org');
    });

    it('explains a failed EP content read, including that it may have been audited', async () => {
        api.getAppMessageBody.mockRejectedValue(httpError(503));
        vf.showConfirm.mockResolvedValue(true);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await button('Show content').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("The message couldn't be read right now");
        expect(wrapper!.text()).toContain('may still have been recorded in the audit log');
    });

    it('explains a failed non-EP content read without audit wording', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        api.getAppMessageBody.mockRejectedValue(httpError(503));
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.text()).toContain("The message couldn't be read right now");
        expect(wrapper!.text()).not.toMatch(/audit/i);
    });

    it('rejects a malformed non-EP content response', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        api.getAppMessageBody.mockResolvedValue({ data: { ...body, to: 'ada@example.org' } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.text()).toContain("Thinpost sent back something the console didn't understand");
        expect(wrapper!.find('pre').exists()).toBe(false);
        expect(wrapper!.find('iframe').exists()).toBe(false);
    });

    it('shows recorded recipients with their delivery numbers, otherwise "Not recorded"', async () => {
        api.getAppMessage.mockResolvedValue({
            data: {
                message: { ...plain, recipients: ['ada@example.org', 'grace@example.net'], recipient_count: 2 },
                deliveries: [
                    { id: 'a'.repeat(32), recipient_index: 0, recipient: 'ada@example.org', state: 'accepted' },
                    { id: 'b'.repeat(32), recipient_index: 1, state: 'uncertain' }
                ]
            }
        });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const rowsText = wrapper!.findAll('tbody tr').map(r => r.findAll('td')[0]!.text());
        expect(rowsText).toEqual(['#1ada@example.org', '#2Not recorded']);
        expect(wrapper!.find('[data-fact="recipients"]').text()).toContain('grace@example.net');
    });

    it('shows To, Cc and Bcc on separate rows and tags Cc and Bcc deliveries', async () => {
        api.getAppMessage.mockResolvedValue({
            data: {
                message: { ...plain, recipients: ['ada@example.org', 'grace@example.net', 'linus@example.com'], recipient_kinds: ['to', 'cc', 'bcc'], recipient_count: 3 },
                deliveries: [
                    { id: 'a'.repeat(32), recipient_index: 0, recipient: 'ada@example.org', recipient_kind: 'to', state: 'accepted' },
                    { id: 'b'.repeat(32), recipient_index: 1, recipient: 'grace@example.net', recipient_kind: 'cc', state: 'accepted' },
                    { id: 'c'.repeat(32), recipient_index: 2, recipient: 'linus@example.com', recipient_kind: 'bcc', state: 'accepted' }
                ]
            }
        });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-fact="recipients"]').text()).toContain('ada@example.org');
        expect(wrapper!.find('[data-fact="recipients"]').text()).not.toContain('grace@example.net');
        expect(wrapper!.find('[data-fact="recipients-cc"]').text()).toContain('grace@example.net');
        expect(wrapper!.find('[data-fact="recipients-cc"] dt').text()).toBe('Cc');
        expect(wrapper!.find('[data-fact="recipients-bcc"]').text()).toContain('linus@example.com');
        expect(wrapper!.find('[data-fact="recipients-bcc"] dt').text()).toBe('Bcc');
        const tags = wrapper!.findAll('tbody tr').map(r => (r.find('[data-field="recipient-kind"]').exists() ? r.find('[data-field="recipient-kind"]').text() : ''));
        expect(tags).toEqual(['', 'Cc', 'Bcc']);
    });

    it('shows no Cc or Bcc rows when every recipient is a To recipient', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: { ...plain, recipients: ['ada@example.org'], recipient_kinds: ['to'], recipient_count: 1 }, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-fact="recipients-cc"]').exists()).toBe(false);
        expect(wrapper!.find('[data-fact="recipients-bcc"]').exists()).toBe(false);
        expect(wrapper!.find('[data-field="recipient-kind"]').exists()).toBe(false);
    });

    it('collapses a long recipient list and expands it on request', async () => {
        const many = Array.from({ length: 12 }, (_, i) => `u${i}@example.org`);
        api.getAppMessage.mockResolvedValue({ data: { message: { ...plain, recipients: many, recipient_count: 12 }, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const fact = () => wrapper!.find('[data-fact="recipients"]');
        expect(fact().find('.v').text().split(',')).toHaveLength(10);
        await fact().findAll('button').find(b => b.text().includes('+2 more'))!.trigger('click');
        expect(fact().find('.v').text().split(',')).toHaveLength(12);
    });

    it('shows the sender domain for plain and protected messages, or "Not recorded"', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-fact="sender-domain"]').text()).toContain('example.com');
        wrapper!.unmount();

        const { sender_domain: _omit, ...noDomain } = plain;
        api.getAppMessage.mockResolvedValue({ data: { message: noDomain, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-fact="sender-domain"]').text()).toContain('Not recorded');
    });

    it('shows the recorded From with its name for protected messages, the bare address, or "Not recorded"', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: { ...summary, sender: { address: 'Billing@example.com', name: 'Acme Billing' } }, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-fact="sender"]').text()).toContain('Acme Billing <Billing@example.com>');
        wrapper!.unmount();

        api.getAppMessage.mockResolvedValue({ data: { message: { ...summary, sender: { address: 'Billing@example.com' } }, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-fact="sender"]').text()).toContain('Billing@example.com');
        expect(wrapper!.find('[data-fact="sender"]').text()).not.toContain('<');
        wrapper!.unmount();

        api.getAppMessage.mockResolvedValue({ data: { message: summary, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-fact="sender"]').text()).toContain('Not recorded');
    });

    it('starts an in-memory sender-domain search in the messages list without touching the URL', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        api.listAppMessages.mockResolvedValue({ data: { messages: [] } });
        api.searchAppMessages.mockResolvedValue({ data: { messages: [] } });
        const { default: AppMessages } = await import('@/screens/app-messages.vue');
        const mounted = await mountAppRoute(`/apps/${APP_ID}/messages/${MESSAGE_ID}`, { 'app-message': MessageDetail, 'app-messages': AppMessages });
        wrapper = mounted.wrapper;
        await button('Find messages from this domain').trigger('click');
        await flushPromises();
        expect(mounted.router.currentRoute.value.name).toBe('app-messages');
        expect(mounted.router.currentRoute.value.fullPath).toBe(`/apps/${APP_ID}/messages`);
        expect(api.searchAppMessages).toHaveBeenCalledWith(
            expect.objectContaining({ body: { q: 'example.com', field: 'sender_domain', limit: 50 } })
        );
    });

    it('copies the delivery ID from a labelled button and shows its short form', async () => {
        const writeText = vi.fn().mockResolvedValue(undefined);
        Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const copy = wrapper!.find('tr[data-delivery="' + 'a'.repeat(32) + '"] button[aria-label="Copy delivery ID for recipient #1"]');
        expect(copy.attributes('title')).toBe('Copy delivery ID');
        expect(wrapper!.find('tr[data-delivery="' + 'a'.repeat(32) + '"] code.did').text()).toBe('a'.repeat(12));
        await copy.trigger('click');
        await flushPromises();
        expect(writeText).toHaveBeenCalledWith('a'.repeat(32));
        expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Copied delivery ID' }));
    });

    describe('Message-ID header', () => {
        const shared = `<${MESSAGE_ID}@example.com>`;
        const fact = () => wrapper!.find('[data-fact="message-id-header"]');

        it('shows and copies the shared header without leaving it in the route or storage', async () => {
            const writeText = vi.fn().mockResolvedValue(undefined);
            Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
            api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries: deliveries.map(d => ({ ...d, message_id_header: shared })) } });
            const mounted = await mountAppRoute(`/apps/${APP_ID}/messages/${MESSAGE_ID}`, { 'app-message': MessageDetail });
            wrapper = mounted.wrapper;
            expect(fact().find('code').text()).toBe(shared);
            await fact().find('button').trigger('click');
            await flushPromises();
            expect(writeText).toHaveBeenCalledWith(shared);
            expect(vf.showToast).toHaveBeenCalledWith(expect.objectContaining({ message: 'Copied Message-ID header' }));
            expect(JSON.stringify(mounted.router.currentRoute.value)).not.toContain('@example.com');
            expect(JSON.stringify(history.state ?? null)).not.toContain('@example.com');
            expect(JSON.stringify({ ...sessionStorage })).not.toContain('@example.com');
            expect(JSON.stringify({ ...localStorage })).not.toContain('@example.com');
        });

        it('says different for each recipient for a message sent before shared Message-IDs', async () => {
            api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries: deliveries.map(d => ({ ...d, message_id_header: `<${d.id}@example.com>` })) } });
            await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
            expect(fact().text()).toContain('Different for each recipient');
            expect(fact().find('button').exists()).toBe(false);
        });

        it('says not assigned yet before there are deliveries, and not recorded without a value', async () => {
            api.getAppMessage.mockResolvedValue({ data: { message: { ...plain, state: 'queued' }, deliveries: [] } });
            await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
            expect(fact().text()).toContain('Not assigned yet');
            wrapper!.unmount();
            api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
            await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
            expect(fact().text()).toContain('Not recorded');
        });
    });

    it('shows the preparation state only when it is not prepared', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-fact="preparation"]').exists()).toBe(false);
        wrapper!.unmount();

        api.getAppMessage.mockResolvedValue({ data: { message: { ...summary, state: 'queued' }, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-fact="preparation"]').text()).toContain('Queued');
    });

    it('says subject not recorded when the subject is missing', async () => {
        const { subject: _s, ...bare } = plain;
        api.getAppMessage.mockResolvedValue({ data: { message: bare, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('#message-heading').text()).toBe('Subject: Subject not recorded');
    });

    it('shows not found for a 404', async () => {
        api.getAppMessage.mockRejectedValue(httpError(404));
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.text()).toContain('Message not found');
    });

    it('shows an unrecognized delivery state verbatim without offering an action', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: summary, deliveries: [{ id: 'c'.repeat(32), recipient_index: 0, state: 'bounced_later' }] } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('tbody').text()).toContain('bounced_later');
        expect(wrapper!.findAll('button').some(b => b.text().startsWith('Retry'))).toBe(false);
    });
});

describe('delivery attempt details', () => {
    const QUEUED_ID = 'c'.repeat(32);
    const ACCEPTED_ID = 'd'.repeat(32);
    const DENIED_ID = 'e'.repeat(32);
    const FRESH_ID = 'f'.repeat(32);
    const NEXT = '2026-11-02T15:30:00Z';

    function withDeliveries(items: unknown[]) {
        api.getAppMessage.mockResolvedValue({ data: { message: { ...summary, delivery_count: items.length }, deliveries: items } });
    }

    function cell(deliveryId: string, field: string) {
        return wrapper!.find(`tr[data-delivery="${deliveryId}"] td[data-field="${field}"]`);
    }

    function headers() {
        return wrapper!.findAll('thead th').map(th => th.text());
    }

    async function helpText() {
        return openHelp(wrapper!, 'deliveries');
    }

    it('omits the attempt columns for a server that does not report them', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(headers()).toEqual(['Recipient', 'Status', 'Actions']);
        expect(wrapper!.find('td[data-field]').exists()).toBe(false);
        expect(await helpText()).toContain("Attempt counts and timing aren't available for this message");
        expect(wrapper!.text()).not.toContain('None completed');
    });

    it('shows retry count, earliest eligibility and the last completed attempt', async () => {
        withDeliveries([
            { id: QUEUED_ID, recipient_index: 0, state: 'queued', retry_attempts: 2, next_attempt_at: NEXT, last_completed_outcome: 'rejected', last_smtp_code: 451 },
            { id: ACCEPTED_ID, recipient_index: 1, state: 'accepted', retry_attempts: 1, last_completed_outcome: 'accepted', last_smtp_code: 250 }
        ]);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);

        expect(headers()).toEqual(['Recipient', 'Status', 'Attempts', 'Last result', 'Next attempt', 'Actions']);
        expect(cell(QUEUED_ID, 'retry-attempts').text()).toBe('2');
        const next = cell(QUEUED_ID, 'next-attempt');
        expect(next.find('time').attributes('datetime')).toBe(NEXT);
        expect(next.find('[title]').attributes('title')).toContain('Not before');
        expect(next.find('[title]').attributes('title')).toContain('The actual attempt may be later');
        expect(cell(QUEUED_ID, 'last-attempt').text()).toBe('451 · temporary');
        expect(cell(QUEUED_ID, 'last-attempt').find('[title]').attributes('title')).toContain('Rejected');

        // Not queued: no eligibility, even if a time were sent.
        expect(cell(ACCEPTED_ID, 'next-attempt').text()).toContain('Not queued');
        expect(cell(ACCEPTED_ID, 'next-attempt').find('time').exists()).toBe(false);
        const accepted = cell(ACCEPTED_ID, 'last-attempt');
        expect(accepted.text()).toBe('Accepted · 250');
        expect(accepted.text()).not.toMatch(/deliver/i);
        expect(accepted.find('[title]').attributes('title')).toContain('Accepted by mail server');
        expect(accepted.find('[title]').attributes('title')).toContain("doesn't guarantee it reached the inbox");
    });

    it('shows a relative next-eligible time, "now" once due, and the outcome with its code', async () => {
        const soon = new Date(Date.now() + 35 * 60_000).toISOString();
        withDeliveries([
            { id: QUEUED_ID, recipient_index: 0, state: 'queued', retry_attempts: 1, next_attempt_at: soon, last_completed_outcome: 'route_rejected', last_smtp_code: 451 },
            { id: FRESH_ID, recipient_index: 1, state: 'queued', retry_attempts: 1, next_attempt_at: '2020-01-01T00:00:00Z', last_completed_outcome: 'novel_outcome', last_smtp_code: 451 }
        ]);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(headers()).toContain('Next attempt');
        expect(cell(QUEUED_ID, 'next-attempt').text()).toMatch(/35 min/);
        expect(cell(FRESH_ID, 'next-attempt').text()).toBe('now');
        expect(cell(FRESH_ID, 'next-attempt').find('[title]').attributes('title')).toContain('Not before');
        expect(cell(QUEUED_ID, 'last-attempt').text()).toBe('One server rejected · 451');
        expect(cell(FRESH_ID, 'last-attempt').text()).toBe('Unrecognized outcome · 451');
    });

    it('explains what the retry count and eligibility time do and do not mean', async () => {
        withDeliveries([{ id: QUEUED_ID, recipient_index: 0, state: 'queued', retry_attempts: 0, next_attempt_at: NEXT }]);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const text = await helpText();
        expect(text).toContain('including one in progress');
        expect(text).toContain("Holds caused by recipient rules aren't counted");
        expect(text).toContain('not when it will actually be sent');
        expect(text).toContain("reply text isn't shown");
    });

    it('says no SMTP response was recorded for code zero and shows a delivery with no completed attempt', async () => {
        withDeliveries([
            { id: DENIED_ID, recipient_index: 0, state: 'recipient_policy_held', retry_attempts: 0, last_completed_outcome: 'recipient_policy_denied', last_smtp_code: 0 },
            { id: FRESH_ID, recipient_index: 1, state: 'queued', retry_attempts: 0, next_attempt_at: NEXT }
        ]);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);

        const denied = cell(DENIED_ID, 'last-attempt').text();
        expect(denied).toBe('Blocked by recipient rules');
        const deniedTitle = cell(DENIED_ID, 'last-attempt').find('[title]').attributes('title')!;
        expect(deniedTitle).toContain('No reply recorded');
        expect(deniedTitle).not.toContain('SMTP 0');
        expect(cell(DENIED_ID, 'retry-attempts').text()).toBe('0');
        expect(cell(FRESH_ID, 'last-attempt').text()).toBe('None completed');
        expect(cell(FRESH_ID, 'retry-attempts').text()).toBe('0');
    });

    it('shows malformed values as not reported and never echoes response text', async () => {
        withDeliveries([
            {
                id: QUEUED_ID,
                recipient_index: 0,
                state: 'queued',
                retry_attempts: -1,
                next_attempt_at: 'soon <b>ada@example.org</b>',
                last_completed_outcome: '550 5.1.1 <ada@example.org> User unknown',
                last_smtp_code: 999,
                last_smtp_text: 'Mailbox full for grace@example.net'
            },
            { id: ACCEPTED_ID, recipient_index: 1, state: 'failed', retry_attempts: 3, last_completed_outcome: 'greylisted_locally', last_smtp_code: 2.5 },
            { id: FRESH_ID, recipient_index: 2, state: 'queued' }
        ]);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);

        expect(cell(QUEUED_ID, 'retry-attempts').text()).toBe('Not reported');
        expect(cell(QUEUED_ID, 'next-attempt').text()).toBe('Not reported');
        expect(cell(QUEUED_ID, 'last-attempt').text()).toContain('Unrecognized outcome');
        expect(cell(QUEUED_ID, 'last-attempt').find('[title]').attributes('title')).toContain('No SMTP code was recorded');
        // An unknown outcome identifier, however well-formed, gets the generic label and is never
        // echoed; a non-integer code is not shown either.
        expect(cell(ACCEPTED_ID, 'last-attempt').text()).toContain('Unrecognized outcome');
        expect(cell(ACCEPTED_ID, 'last-attempt').text()).not.toContain('greylisted_locally');
        expect(cell(ACCEPTED_ID, 'last-attempt').find('[title]').attributes('title')).toContain('No SMTP code was recorded');
        // A row without the fields in an otherwise current response claims nothing.
        expect(cell(FRESH_ID, 'retry-attempts').text()).toBe('Not reported');
        expect(cell(FRESH_ID, 'next-attempt').text()).toBe('Not reported');
        expect(cell(FRESH_ID, 'last-attempt').text()).toBe('Not reported');

        const html = document.body.innerHTML;
        for (const leaked of ['@example', 'User unknown', 'Mailbox full', '550 5.1.1', '<b>', 'greylisted_locally']) expect(html).not.toContain(leaked);
        expect(api.getAppMessageBody).not.toHaveBeenCalled();
    });

    it('shows the attempt details alongside the hold actions', async () => {
        withDeliveries([{ id: DENIED_ID, recipient_index: 0, state: 'recipient_policy_held', retry_attempts: 1, last_completed_outcome: 'recipient_policy_denied', last_smtp_code: 0 }]);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.findAll('thead th')).toHaveLength(6);
        expect(cell(DENIED_ID, 'retry-attempts').text()).toBe('1');
        expect(wrapper!.find(`tr[data-delivery="${DENIED_ID}"]`).text()).toContain('Retry');
    });

    it('updates the attempt details when the deliveries are refreshed', async () => {
        withDeliveries([{ id: QUEUED_ID, recipient_index: 0, state: 'queued', retry_attempts: 1, next_attempt_at: NEXT, last_completed_outcome: 'rejected', last_smtp_code: 421 }]);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(cell(QUEUED_ID, 'retry-attempts').text()).toBe('1');

        withDeliveries([{ id: QUEUED_ID, recipient_index: 0, state: 'in_flight', retry_attempts: 2, last_completed_outcome: 'rejected', last_smtp_code: 421 }]);
        await button('Refresh').trigger('click');
        await flushPromises();
        // The in-flight attempt is counted; the last completed attempt is still the earlier one.
        expect(cell(QUEUED_ID, 'retry-attempts').text()).toBe('2');
        expect(cell(QUEUED_ID, 'next-attempt').text()).toContain('Not queued');
        expect(cell(QUEUED_ID, 'last-attempt').text()).toBe('421 · temporary');
    });
});

describe('recipient rule holds', () => {
    const HELD_ID = 'c'.repeat(32);
    const held = { id: HELD_ID, recipient_index: 2, state: 'recipient_policy_held' };
    const heldDeliveries = [...deliveries, held];
    const heldSummary = { ...summary, delivery_count: 3 };
    const requeued = { data: undefined, response: new Response(null, { status: 204 }) };

    function withDeliveries(items: unknown[]) {
        api.getAppMessage.mockResolvedValue({ data: { message: heldSummary, deliveries: items } });
    }

    function row(deliveryId: string) {
        return wrapper!.find(`tr[data-delivery="${deliveryId}"]`);
    }

    function retryButton(deliveryId = HELD_ID) {
        const match = row(deliveryId)
            .findAll('button')
            .find(b => b.text().startsWith('Retry'));
        if (!match) throw new Error('retry button not found');
        return match;
    }

    function hasRetry(deliveryId: string) {
        return row(deliveryId)
            .findAll('button')
            .some(b => b.text().startsWith('Retry'));
    }

    function deliveriesSection() {
        return wrapper!.find('section[aria-labelledby="deliveries-heading"]');
    }

    async function openRetry() {
        await retryButton().trigger('click');
        await flushPromises();
    }

    async function acknowledgeAndSubmit() {
        await wrapper!.find('input[type="checkbox"]').setValue(true);
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
    }

    beforeEach(() => withDeliveries(heldDeliveries));

    it('labels held deliveries, counts them and explains the hold without revealing recipients', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(row(HELD_ID).text()).toContain('Held by recipient rules');
        expect(row(HELD_ID).text()).toContain('#3');
        const counts = wrapper!.find('ul[aria-label="Delivery counts"]').text();
        expect(counts).toContain('1 held by rules');
        expect(counts).not.toContain('failed');
        expect(deliveriesSection().text()).toContain('held by recipient rules');
        const text = await openHelp(wrapper!, 'recipient rule holds');
        expect(text).toContain("The recipient rules set by your Thinpost administrator don't allow this recipient");
        expect(text).toContain("Held deliveries aren't retried automatically");
        expect(text).not.toContain('before any SMTP connection');
        expect(text).toContain("A retry can't override the rules");
        expect(text).toContain('goes back on hold');
        // Only held rows get the action; no source or address is fetched to show it.
        expect(hasRetry(HELD_ID)).toBe(true);
        expect(hasRetry('a'.repeat(32))).toBe(false);
        expect(hasRetry('b'.repeat(32))).toBe(false);
        expect(api.getAppMessageBody).not.toHaveBeenCalled();
        expect(document.body.innerHTML).not.toContain('@example');
    });

    it('omits the hold notice when nothing is held', async () => {
        withDeliveries(deliveries);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(deliveriesSection().text()).not.toContain("A retry can't override the rules");
        expect(wrapper!.findAll('thead th')).toHaveLength(3);
    });

    it('requires an acknowledged confirmation that states the limits of a retry', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openRetry();

        const dialog = wrapper!.find('[role="alertdialog"]');
        const text = dialog.text();
        expect(text).toContain(APP_ID);
        expect(text).toContain('Billing');
        expect(text).toContain(MESSAGE_ID);
        expect(text).toContain(HELD_ID);
        expect(text).toContain('#3 in the message');
        expect(text).toContain("It doesn't override the recipient rules");
        expect(text).toContain('Thinpost checks the rules again');
        expect(text).toContain('goes back on hold');
        expect(text).toContain("doesn't work if the delivery is no longer held or the app is paused");
        expect(text).toContain('recorded in the audit log');
        expect(text).not.toContain('@example');

        const submit = dialog.findAll('button').find(b => b.text().startsWith('Retry delivery'))!;
        expect(submit.attributes('disabled')).toBeDefined();
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.retryDeliveryPolicyHold).not.toHaveBeenCalled();

        await dialog.findAll('button').find(b => b.text() === 'Cancel')!.trigger('click');
        await flushPromises();
        expect(wrapper!.find('[role="alertdialog"]').exists()).toBe(false);
        expect(api.retryDeliveryPolicyHold).not.toHaveBeenCalled();
    });

    it('requeues the selected delivery and reloads without claiming it was sent', async () => {
        api.retryDeliveryPolicyHold.mockResolvedValue(requeued);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openRetry();

        withDeliveries([...deliveries, { ...held, state: 'queued' }]);
        await acknowledgeAndSubmit();

        expect(api.retryDeliveryPolicyHold).toHaveBeenCalledTimes(1);
        expect(api.retryDeliveryPolicyHold).toHaveBeenCalledWith({ path: { appId: APP_ID, messageId: MESSAGE_ID, deliveryId: HELD_ID } });
        const toast = vf.showToast.mock.calls[0]?.[0].message as string;
        expect(toast).toContain('requeued');
        expect(toast).toContain('goes back on hold');
        expect(toast).not.toMatch(/\b(sent|delivered|accepted)\b/i);
        expect(api.getAppMessage).toHaveBeenCalledTimes(2);
        expect(api.listApps).toHaveBeenCalledTimes(2);
        expect(wrapper!.find('[role="alertdialog"]').exists()).toBe(false);
        expect(row(HELD_ID).text()).toContain('Queued');
        expect(hasRetry(HELD_ID)).toBe(false);
    });

    it('shows a delivery that returned to the hold after a successful requeue', async () => {
        api.retryDeliveryPolicyHold.mockResolvedValue(requeued);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openRetry();
        await acknowledgeAndSubmit();
        expect(row(HELD_ID).text()).toContain('Held by recipient rules');
        expect(retryButton().attributes('disabled')).toBeUndefined();
    });

    it.each([
        [409, 'Nothing was requeued', 'no longer held by recipient rules'],
        [404, "That delivery wasn't found for this message", 'Nothing was requeued']
    ])('explains a %i and reloads the deliveries', async (status, first, second) => {
        api.retryDeliveryPolicyHold.mockRejectedValue(httpError(status));
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openRetry();
        await acknowledgeAndSubmit();

        const text = deliveriesSection().text();
        expect(text).toContain(first);
        expect(text).toContain(second);
        expect(text).not.toContain("We can't tell whether the change went through");
        expect(vf.showToast).not.toHaveBeenCalled();
        expect(api.getAppMessage).toHaveBeenCalledTimes(2);
        expect(retryButton().attributes('disabled')).toBeUndefined();
    });

    it('shows a conflict caused by an app that became inactive and blocks retry', async () => {
        api.retryDeliveryPolicyHold.mockRejectedValue(httpError(409));
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openRetry();
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        await acknowledgeAndSubmit();

        expect(deliveriesSection().text()).toContain('app is paused');
        expect(retryButton().attributes('disabled')).toBeDefined();
        expect(row(HELD_ID).text()).toContain('The app is paused.');
    });

    it('reports a definitive failure without blocking or reloading', async () => {
        api.retryDeliveryPolicyHold.mockRejectedValue(httpError(403));
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openRetry();
        await acknowledgeAndSubmit();

        expect(deliveriesSection().text()).toContain("isn't allowed to use this console");
        expect(deliveriesSection().text()).not.toContain("We can't tell whether the change went through");
        expect(api.getAppMessage).toHaveBeenCalledTimes(1);
        expect(retryButton().attributes('disabled')).toBeUndefined();
    });

    it.each([
        ['no response', () => Promise.reject(networkError()), "Couldn't reach Thinpost"],
        ['a 503', () => Promise.reject(httpError(503)), 'Retrying is temporarily unavailable'],
        ['a 502', () => Promise.reject(httpError(502)), 'Thinpost is temporarily unavailable'],
        ['a success other than 204', () => Promise.resolve({ data: '<html>', response: new Response('<html>', { status: 200 }) }), "Thinpost sent back something the console didn't understand"]
    ])('blocks repeat retries after %s until the deliveries are reloaded', async (_label, result, message) => {
        api.retryDeliveryPolicyHold.mockImplementation(result);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openRetry();
        await acknowledgeAndSubmit();

        expect(deliveriesSection().text()).toContain(message);
        expect(deliveriesSection().text()).toContain("We can't tell whether the change went through");
        expect(vf.showToast).not.toHaveBeenCalled();
        // No automatic repeat and no automatic reload.
        expect(api.retryDeliveryPolicyHold).toHaveBeenCalledTimes(1);
        expect(api.getAppMessage).toHaveBeenCalledTimes(1);
        expect(retryButton().attributes('disabled')).toBeDefined();
        expect(row(HELD_ID).text()).toContain('Reload the deliveries first');
        await retryButton().trigger('click');
        await flushPromises();
        expect(wrapper!.find('[role="alertdialog"]').exists()).toBe(false);

        // A failed reload keeps the block.
        api.getAppMessage.mockRejectedValueOnce(networkError());
        await deliveriesSection().findAll('button').find(b => b.text().startsWith('Refresh'))!.trigger('click');
        await flushPromises();
        expect(deliveriesSection().text()).toContain('from the last time they loaded');
        expect(retryButton().attributes('disabled')).toBeDefined();

        await deliveriesSection().findAll('button').find(b => b.text().startsWith('Refresh'))!.trigger('click');
        await flushPromises();
        expect(api.getAppMessage).toHaveBeenCalledTimes(3);
        expect(deliveriesSection().text()).not.toContain("We can't tell whether the change went through");
        expect(retryButton().attributes('disabled')).toBeUndefined();
        expect(api.retryDeliveryPolicyHold).toHaveBeenCalledTimes(1);
    });

    it('does not retry when the session ends while the dialog is open', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openRetry();
        authState.status = 'ended';
        await acknowledgeAndSubmit();
        expect(api.retryDeliveryPolicyHold).not.toHaveBeenCalled();
    });

    it('blocks retry for an inactive app', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(retryButton().attributes('disabled')).toBeDefined();
        expect(row(HELD_ID).text()).toContain('The app is paused.');
        await retryButton().trigger('click');
        await flushPromises();
        expect(wrapper!.find('[role="alertdialog"]').exists()).toBe(false);
    });

    it('ignores a retry response that arrives after leaving the page', async () => {
        let resolveRetry: (value: unknown) => void = () => undefined;
        api.retryDeliveryPolicyHold.mockReturnValue(new Promise(resolve => (resolveRetry = resolve)));
        const mounted = await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openRetry();
        await acknowledgeAndSubmit();
        await mounted.vm.$router.push({ name: 'app-messages', params: { appId: APP_ID } });
        await flushPromises();
        resolveRetry(requeued);
        await flushPromises();
        expect(vf.showToast).not.toHaveBeenCalled();
        expect(api.getAppMessage).toHaveBeenCalledTimes(1);
    });

    it('keeps the content on screen when the deliveries are refreshed', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await button('Refresh').trigger('click');
        await flushPromises();
        expect(api.getAppMessage).toHaveBeenCalledTimes(2);
        expect(api.getAppMessageBody).toHaveBeenCalledTimes(1);
        expect(document.body.innerHTML).toContain('ada@example.org');
    });
});

describe('tracking holds', () => {
    const HELD_ID = 'd'.repeat(32);
    const POLICY_ID = 'e'.repeat(32);
    const held = { id: HELD_ID, recipient_index: 2, state: 'tracking_held' };
    const trackingDeliveries = [...deliveries, held];
    const heldSummary = { ...summary, delivery_count: 3 };
    const marked = { data: undefined, response: new Response(null, { status: 204 }) };

    function withDeliveries(items: unknown[], message: unknown = heldSummary) {
        api.getAppMessage.mockResolvedValue({ data: { message, deliveries: items } });
    }

    function row(deliveryId: string) {
        return wrapper!.find(`tr[data-delivery="${deliveryId}"]`);
    }

    function markButton(deliveryId = HELD_ID) {
        const match = row(deliveryId)
            .findAll('button')
            .find(b => b.text().startsWith('Mark failed'));
        if (!match) throw new Error('mark failed button not found');
        return match;
    }

    function hasMark(deliveryId: string) {
        return row(deliveryId)
            .findAll('button')
            .some(b => b.text().startsWith('Mark failed'));
    }

    function deliveriesSection() {
        return wrapper!.find('section[aria-labelledby="deliveries-heading"]');
    }

    function dialog() {
        return wrapper!.find('[role="alertdialog"]');
    }

    async function openDialog() {
        await markButton().trigger('click');
        await flushPromises();
    }

    async function acknowledgeAndSubmit() {
        await wrapper!.find('input[type="checkbox"]').setValue(true);
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
    }

    async function clickSectionButton(label: string) {
        await deliveriesSection()
            .findAll('button')
            .find(b => b.text().startsWith(label))!
            .trigger('click');
        await flushPromises();
    }

    beforeEach(() => withDeliveries(trackingDeliveries));

    it('labels and counts tracking holds and offers the action only on them', async () => {
        withDeliveries([
            ...trackingDeliveries,
            { id: POLICY_ID, recipient_index: 3, state: 'recipient_policy_held' },
            { id: 'f'.repeat(32), recipient_index: 4, state: 'failed' }
        ]);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(row(HELD_ID).text()).toContain('Held: tracking unavailable');
        expect(row(HELD_ID).text()).toContain('#3');
        expect(wrapper!.find('ul[aria-label="Delivery counts"]').text()).toContain('1 held (tracking)');
        expect(deliveriesSection().text()).toContain('held: tracking unavailable');
        const text = await openHelp(wrapper!, 'tracking holds');
        expect(text).toContain("aren't released or sent automatically");
        expect(text).toContain("doesn't apply to deliveries with an uncertain result");

        expect(markButton().attributes('disabled')).toBeUndefined();
        // Uncertain, accepted, failed and recipient-policy holds never get this action.
        for (const other of ['a'.repeat(32), 'b'.repeat(32), POLICY_ID, 'f'.repeat(32)]) expect(hasMark(other)).toBe(false);
        expect(row(POLICY_ID).text()).toContain('Retry');
        expect(row(HELD_ID).text()).not.toContain('Retry');
        expect(api.getAppMessageBody).not.toHaveBeenCalled();
    });

    it.each(['uncertain', 'failed', 'accepted', 'queued', 'in_flight', 'recipient_policy_held', 'tracking_abandoned', 'TRACKING_HELD'])(
        'never offers the action for a %s delivery',
        async state => {
            withDeliveries([{ id: HELD_ID, recipient_index: 0, state }]);
            await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
            expect(hasMark(HELD_ID)).toBe(false);
            expect(deliveriesSection().text()).not.toContain('held because tracking is unavailable');
            expect(wrapper!.find('ul[aria-label="Delivery counts"]').text()).not.toContain('unavailable tracking');
        }
    );

    it('explains in the confirmation that marking failed replaces the next attempt', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openDialog();

        const text = dialog().text();
        expect(text).toContain('Marks the delivery failed instead of trying to send it again');
        expect(text).not.toContain('before any SMTP connection');
        expect(text).not.toContain('no SMTP server rejected');
        expect(await openHelp(wrapper!, 'tracking holds')).toContain("aren't released or sent automatically");
    });

    it('requires an acknowledged confirmation that states what marking failed does and does not do', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openDialog();

        const text = dialog().text();
        expect(text).toContain('Billing');
        expect(text).toContain(APP_ID);
        expect(text).toContain(MESSAGE_ID);
        expect(text).toContain(HELD_ID);
        expect(text).toContain('#3 in the message');
        expect(text).toContain('Marks the delivery failed instead of trying to send it again');
        expect(text).toContain('Failed is final');
        expect(text).toContain("Doesn't send the message");
        expect(text).toContain("doesn't apply to deliveries with an uncertain result");
        expect(text).toContain('only works while the delivery is still held because tracking is unavailable');
        expect(text).toContain('tracking_abandoned');
        expect(text).not.toContain('app is paused');
        expect(text).not.toContain('@example');

        const submit = dialog()
            .findAll('button')
            .find(b => b.text().startsWith('Mark failed'))!;
        expect(submit.attributes('disabled')).toBeDefined();
        await wrapper!.find('form').trigger('submit');
        await flushPromises();
        expect(api.abandonDeliveryTrackingHold).not.toHaveBeenCalled();

        await dialog()
            .findAll('button')
            .find(b => b.text() === 'Cancel')!
            .trigger('click');
        await flushPromises();
        expect(dialog().exists()).toBe(false);
        expect(api.abandonDeliveryTrackingHold).not.toHaveBeenCalled();
    });

    it('stays available for an inactive app and says its webhook waits', async () => {
        api.listApps.mockResolvedValue({ data: { apps: [{ ...app, active: false }] } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(markButton().attributes('disabled')).toBeUndefined();
        await openDialog();
        expect(dialog().text()).toContain('the webhook event waits until it is resumed');
    });

    it('marks the selected delivery failed and reloads the delivery detail', async () => {
        api.abandonDeliveryTrackingHold.mockResolvedValue(marked);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openDialog();

        withDeliveries([...deliveries, { ...held, state: 'failed' }], { ...heldSummary, failed_count: 1 });
        await acknowledgeAndSubmit();

        expect(api.abandonDeliveryTrackingHold).toHaveBeenCalledTimes(1);
        expect(api.abandonDeliveryTrackingHold).toHaveBeenCalledWith({ path: { appId: APP_ID, messageId: MESSAGE_ID, deliveryId: HELD_ID } });
        const toast = vf.showToast.mock.calls[0]?.[0].message as string;
        expect(toast).toContain('marked failed without sending');
        expect(toast).not.toMatch(/\b(sent|delivered|accepted|resent|requeued)\b/i);
        expect(api.getAppMessage).toHaveBeenCalledTimes(2);
        expect(dialog().exists()).toBe(false);
        expect(row(HELD_ID).text()).toContain('Failed');
        expect(hasMark(HELD_ID)).toBe(false);
        expect(wrapper!.find('ul[aria-label="Delivery counts"]').text()).toContain('1 failed');
        expect(deliveriesSection().text()).not.toContain('held because tracking is unavailable');
        expect(wrapper!.findAll('thead th')).toHaveLength(3);
    });

    it('blocks the stale action when the reload after success fails', async () => {
        api.abandonDeliveryTrackingHold.mockResolvedValue(marked);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openDialog();
        api.getAppMessage.mockRejectedValueOnce(networkError());
        await acknowledgeAndSubmit();

        expect(vf.showToast).toHaveBeenCalledTimes(1);
        expect(deliveriesSection().text()).toContain('from the last time they loaded');
        expect(markButton().attributes('disabled')).toBeDefined();
        expect(row(HELD_ID).text()).toContain('Reload the deliveries first: the deliveries shown may be out of date.');

        withDeliveries([...deliveries, { ...held, state: 'failed' }]);
        await clickSectionButton('Refresh');
        expect(hasMark(HELD_ID)).toBe(false);
        expect(api.abandonDeliveryTrackingHold).toHaveBeenCalledTimes(1);
    });

    it.each([
        [409, 'Nothing was marked failed.', 'no longer held because tracking is unavailable'],
        [404, 'Nothing was marked failed.', "That delivery wasn't found for this message"]
    ])('explains a %i, reloads the deliveries and directs a state check', async (status, first, second) => {
        api.abandonDeliveryTrackingHold.mockRejectedValue(httpError(status));
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openDialog();
        withDeliveries([...deliveries, { ...held, state: 'failed' }]);
        await acknowledgeAndSubmit();

        const text = deliveriesSection().text();
        expect(text).toContain(first);
        expect(text).toContain(second);
        expect(text).toContain('have been reloaded. Check their current state before trying again');
        expect(text).not.toContain("We can't tell whether the change went through");
        expect(vf.showToast).not.toHaveBeenCalled();
        expect(api.getAppMessage).toHaveBeenCalledTimes(2);
        expect(row(HELD_ID).text()).toContain('Failed');
        expect(hasMark(HELD_ID)).toBe(false);
    });

    it('keeps the action blocked after a conflict when the reload also fails', async () => {
        api.abandonDeliveryTrackingHold.mockRejectedValue(httpError(409));
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openDialog();
        api.getAppMessage.mockRejectedValueOnce(httpError(503));
        await acknowledgeAndSubmit();

        const text = deliveriesSection().text();
        expect(text).toContain('Nothing was marked failed.');
        expect(text).toContain('Reload the deliveries to check their current state before trying again');
        expect(markButton().attributes('disabled')).toBeDefined();
        await markButton().trigger('click');
        await flushPromises();
        expect(dialog().exists()).toBe(false);

        await clickSectionButton('Reload deliveries');
        expect(deliveriesSection().text()).not.toContain('Nothing was marked failed');
        expect(markButton().attributes('disabled')).toBeUndefined();
        expect(api.abandonDeliveryTrackingHold).toHaveBeenCalledTimes(1);
    });

    it.each([
        ['no response', () => Promise.reject(networkError()), "Couldn't reach Thinpost"],
        ['a 503', () => Promise.reject(httpError(503)), 'Marking this delivery failed is temporarily unavailable'],
        ['a 502', () => Promise.reject(httpError(502)), 'Thinpost is temporarily unavailable'],
        ['a 500', () => Promise.reject(httpError(500)), 'Something went wrong'],
        ['a 405', () => Promise.reject(httpError(405)), 'Something went wrong'],
        ['a success other than 204', () => Promise.resolve({ data: '<html>', response: new Response('<html>', { status: 200 }) }), "Thinpost sent back something the console didn't understand"]
    ])('blocks the action after %s until the deliveries are reloaded', async (_label, result, message) => {
        api.abandonDeliveryTrackingHold.mockImplementation(result);
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openDialog();
        await acknowledgeAndSubmit();

        const text = deliveriesSection().text();
        expect(text).toContain(message);
        expect(text).toContain("We can't tell whether the change went through. Reload to check before trying again");
        expect(vf.showToast).not.toHaveBeenCalled();
        // No automatic repeat and no automatic reload.
        expect(api.abandonDeliveryTrackingHold).toHaveBeenCalledTimes(1);
        expect(api.getAppMessage).toHaveBeenCalledTimes(1);
        expect(markButton().attributes('disabled')).toBeDefined();
        expect(row(HELD_ID).text()).toContain('Reload the deliveries first: an earlier action may or may not have gone through.');
        await markButton().trigger('click');
        await flushPromises();
        expect(dialog().exists()).toBe(false);

        // A failed reload keeps the block.
        api.getAppMessage.mockRejectedValueOnce(networkError());
        await clickSectionButton('Refresh');
        expect(deliveriesSection().text()).toContain('from the last time they loaded');
        expect(markButton().attributes('disabled')).toBeDefined();

        // A successful reload shows the real state and lifts the block.
        await clickSectionButton('Refresh');
        expect(api.getAppMessage).toHaveBeenCalledTimes(3);
        expect(deliveriesSection().text()).not.toContain("We can't tell whether the change went through");
        expect(markButton().attributes('disabled')).toBeUndefined();
        expect(api.abandonDeliveryTrackingHold).toHaveBeenCalledTimes(1);
    });

    it('also blocks recipient-policy retries while an outcome is unknown', async () => {
        withDeliveries([...trackingDeliveries, { id: POLICY_ID, recipient_index: 3, state: 'recipient_policy_held' }]);
        api.abandonDeliveryTrackingHold.mockRejectedValue(networkError());
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openDialog();
        await acknowledgeAndSubmit();
        const retry = row(POLICY_ID)
            .findAll('button')
            .find(b => b.text().startsWith('Retry'))!;
        expect(retry.attributes('disabled')).toBeDefined();
    });

    it('reports a definitive refusal without blocking or reloading', async () => {
        api.abandonDeliveryTrackingHold.mockRejectedValue(httpError(403));
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openDialog();
        await acknowledgeAndSubmit();

        expect(deliveriesSection().text()).toContain("isn't allowed to use this console");
        expect(deliveriesSection().text()).not.toContain("We can't tell whether the change went through");
        expect(api.getAppMessage).toHaveBeenCalledTimes(1);
        expect(markButton().attributes('disabled')).toBeUndefined();
    });

    it('does not act when the session ends while the dialog is open', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openDialog();
        authState.status = 'ended';
        await acknowledgeAndSubmit();
        expect(api.abandonDeliveryTrackingHold).not.toHaveBeenCalled();
    });

    it('ignores a response that arrives after leaving the page', async () => {
        let resolve: (value: unknown) => void = () => undefined;
        api.abandonDeliveryTrackingHold.mockReturnValue(new Promise(r => (resolve = r)));
        const mounted = await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await openDialog();
        await acknowledgeAndSubmit();
        await mounted.vm.$router.push({ name: 'app-messages', params: { appId: APP_ID } });
        await flushPromises();
        resolve(marked);
        await flushPromises();
        expect(vf.showToast).not.toHaveBeenCalled();
        expect(api.getAppMessage).toHaveBeenCalledTimes(1);
    });
});

describe('delivery headers', () => {
    const HEADERS = [
        { name: 'Received', value: 'from a by b' },
        { name: 'Subject', value: '=?UTF-8?B?SGk=?=', decoded_value: 'Hi' },
        { name: 'To', value: 'ada@example.org' }
    ];

    function headersButton(index = 0) {
        return wrapper!.findAll('tbody tr')[index]!.findAll('button').find(b => b.text().startsWith('Headers'))!;
    }

    beforeEach(() => {
        api.getAppDeliveryHeaders.mockResolvedValue({ data: { headers: HEADERS } });
    });

    it('opens without a dialog for non-EP and shows every header in order as raw text', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const before = openModalCount.value;
        await headersButton().trigger('click');
        await flushPromises();
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(api.getAppDeliveryHeaders).toHaveBeenCalledWith({ path: { appId: APP_ID, messageId: MESSAGE_ID, deliveryId: 'a'.repeat(32) } });
        expect(openModalCount.value).toBe(before + 1);

        const modal = wrapper!.find('[role="dialog"]');
        const raw = modal.find('pre.headers-raw');
        expect(raw.text()).toBe('Received: from a by b\nSubject: =?UTF-8?B?SGk=?=\nTo: ada@example.org');
        expect(modal.find('table').exists()).toBe(false);
        expect(modal.text()).not.toMatch(/audit/i);

        await modal.findAll('button').find(b => b.text() === 'Close')!.trigger('click');
        await flushPromises();
        expect(wrapper!.find('[role="dialog"]').exists()).toBe(false);
        expect(openModalCount.value).toBe(before);
    });

    it('notes that a Bcc copy is not listed in To or Cc', async () => {
        api.getAppMessage.mockResolvedValue({
            data: {
                message: { ...plain, recipients: ['ada@example.org', 'linus@example.com'], recipient_kinds: ['to', 'bcc'], recipient_count: 2 },
                deliveries: [
                    { id: 'a'.repeat(32), recipient_index: 0, recipient: 'ada@example.org', recipient_kind: 'to', state: 'accepted' },
                    { id: 'b'.repeat(32), recipient_index: 1, recipient: 'linus@example.com', recipient_kind: 'bcc', state: 'accepted' }
                ]
            }
        });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await headersButton(0).trigger('click');
        await flushPromises();
        expect(wrapper!.find('[role="dialog"] [data-field="bcc-note"]').exists()).toBe(false);
        await wrapper!.find('[role="dialog"]').findAll('button').find(b => b.text() === 'Close')!.trigger('click');
        await flushPromises();
        await headersButton(1).trigger('click');
        await flushPromises();
        expect(wrapper!.find('[role="dialog"] [data-field="bcc-note"]').text()).toBe('Bcc copy: sent as a blind copy; the headers list To and Cc only.');
    });

    it('confirms with the audit log first for Enhanced Protection', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        vf.showConfirm.mockResolvedValueOnce(false);
        await headersButton().trigger('click');
        await flushPromises();
        expect(vf.showConfirm).toHaveBeenCalledWith(expect.any(String), expect.stringContaining('audit log'));
        expect(api.getAppDeliveryHeaders).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(true);
        await headersButton().trigger('click');
        await flushPromises();
        expect(api.getAppDeliveryHeaders).toHaveBeenCalledTimes(1);
        expect(wrapper!.find('[role="dialog"]').exists()).toBe(true);
    });

    it('explains a 404 and a 503', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        api.getAppDeliveryHeaders.mockRejectedValueOnce(httpError(404)).mockRejectedValueOnce(httpError(503));
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await headersButton().trigger('click');
        await flushPromises();
        expect(wrapper!.find('[role="dialog"]').text()).toContain("hasn't been prepared yet");
        await wrapper!.find('[role="dialog"]').findAll('button').find(b => b.text() === 'Close')!.trigger('click');
        await flushPromises();
        await headersButton().trigger('click');
        await flushPromises();
        expect(wrapper!.find('[role="dialog"]').text()).toContain("couldn't be read right now");
        expect(wrapper!.find('[role="dialog"]').text()).not.toMatch(/audit/i);
    });

    it('discards the headers when the session ends', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: plain, deliveries } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        await headersButton().trigger('click');
        await flushPromises();
        expect(document.body.innerHTML).toContain('from a by b');
        authState.status = 'ended';
        await flushPromises();
        expect(document.body.innerHTML).not.toContain('from a by b');
        expect(wrapper!.find('[role="dialog"]').text()).toContain('headers were cleared');
    });
});

describe('message detail engagement', () => {
    it('shows recorded opens and clicks only for tracked deliveries', async () => {
        api.getAppMessage.mockResolvedValue({
            data: {
                message: plain,
                deliveries: [
                    { ...deliveries[0], tracking: { opens: 3, clicks: 1, last_event_at: '2026-10-03T10:00:00Z' } },
                    { ...deliveries[1], tracking: { opens: 0, clicks: 0 } },
                    { id: 'c'.repeat(32), recipient_index: 2, state: 'accepted' }
                ]
            }
        });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const cells = wrapper!.findAll('[data-field="engagement"]').map(c => c.text());
        expect(cells[0]).toMatch(/^Opens 3 · Clicks 1 · last /);
        expect(cells[1]).toBe('Opens 0 · Clicks 0 · none yet');
        expect(cells[2]).toContain('Not tracked');
        expect(await openHelp(wrapper!, 'engagement')).toContain('scanners and privacy proxies');
    });

    it('omits the column when no delivery is tracked', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-field="engagement"]').exists()).toBe(false);
        expect(wrapper!.text()).not.toContain('Engagement');
    });
});

describe('message detail recipient feedback', () => {
    it('tags an unsubscribed delivery with its time and invents nothing for the others', async () => {
        api.getAppMessage.mockResolvedValue({
            data: {
                message: summary,
                deliveries: [
                    { ...deliveries[0], feedback: [{ kind: 'unsubscribe', channel: 'one_click', received_at: '2026-10-03T08:00:00Z' }] },
                    { ...deliveries[1], feedback: [{ kind: 'bogus', channel: 'x', received_at: '2026-10-03T08:00:00Z' }, { kind: 'spam_complaint', channel: 'arf' }] }
                ]
            }
        });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const tagged = wrapper!.find(`tr[data-delivery="${'a'.repeat(32)}"]`);
        expect(tagged.find('[data-field="feedback"]').text()).toBe('Unsubscribed');
        expect(tagged.find('[data-field="feedback"]').attributes('title')).toContain('Unsubscribed');
        expect(wrapper!.find(`tr[data-delivery="${'b'.repeat(32)}"] [data-field="feedback"]`).exists()).toBe(false);
    });

    it('tags a complained delivery with its time', async () => {
        api.getAppMessage.mockResolvedValue({
            data: { message: summary, deliveries: [{ ...deliveries[0], feedback: [{ kind: 'spam_complaint', channel: 'arf', received_at: '2026-10-03T08:00:00Z' }] }, deliveries[1]] }
        });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const tag = wrapper!.find(`tr[data-delivery="${'a'.repeat(32)}"] [data-field="feedback"]`);
        expect(tag.text()).toBe('Complained');
        expect(tag.classes()).toContain('danger');
        expect(tag.attributes('title')).toContain('Complained');
    });
});

describe('message detail hold', () => {
    const held = { ...plain, state: 'on_hold', status: 'on_hold', delivery_count: 0, accepted_count: 0, uncertain_count: 0 };

    it('shows an open hold with its expiry and no release action', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: held, deliveries: [], hold: { expires_at: '2026-10-09T12:00:00Z' } } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('#message-heading').element.parentElement!.querySelector('.tag')!.textContent).toContain('On hold by app');
        const fact = wrapper!.find('[data-fact="hold"]');
        expect(fact.text()).toContain('Until released, or expires');
        expect(fact.find('time').attributes('datetime')).toBe('2026-10-09T12:00:00Z');
        expect(wrapper!.find('[data-fact="message-id-header"] dd').text()).toBe('Not assigned yet');
        expect(wrapper!.findAll('button').some(b => /release|cancel/i.test(b.text()))).toBe(false);
    });

    it.each([
        ['released', 'queued', 'queued', 'Released'],
        ['cancelled', 'cancelled', 'cancelled', 'Cancelled'],
        ['expired', 'hold_expired', 'expired', 'Expired']
    ])('shows a %s hold', async (resolution, state, status, label) => {
        api.getAppMessage.mockResolvedValue({
            data: { message: { ...held, state, status }, deliveries: [], hold: { expires_at: '2026-10-09T12:00:00Z', resolution, resolved_at: '2026-10-03T12:00:00Z' } }
        });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const fact = wrapper!.find('[data-fact="hold"]');
        expect(fact.text()).toContain(label);
        expect(fact.find('time').attributes('datetime')).toBe('2026-10-03T12:00:00Z');
        if (resolution !== 'released') expect(wrapper!.find('[data-fact="message-id-header"] dd').text()).toBe('None (not sent)');
    });

    it('shows a held message quarantined by a recovery as such', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: { ...held, state: 'recovery_hold', status: 'held' }, deliveries: [], hold: { expires_at: '2026-10-09T12:00:00Z' } } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        const fact = wrapper!.find('[data-fact="hold"]');
        expect(fact.text()).toContain('Quarantined after data recovery');
        expect(fact.text()).not.toContain('Until released');
    });

    it('has no hold fact for a message not submitted on hold', async () => {
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-fact="hold"]').exists()).toBe(false);
    });

    it('treats a malformed hold as an invalid response', async () => {
        api.getAppMessage.mockResolvedValue({ data: { message: held, deliveries: [], hold: { resolution: 'released' } } });
        await mountAt(`/apps/${APP_ID}/messages/${MESSAGE_ID}`);
        expect(wrapper!.find('[data-fact="hold"]').exists()).toBe(false);
    });
});
