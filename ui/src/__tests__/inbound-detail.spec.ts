import { flushPromises, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError } from './helpers';
import { mountAppRoute } from './mount-app';

const api = vi.hoisted(() => ({
    listApps: vi.fn(),
    getInboundMessage: vi.fn(),
    getInboundMessageContent: vi.fn(),
    downloadInboundMessageRaw: vi.fn(),
    listInboundMessageWebhookDeliveries: vi.fn(),
    listAppWebhooks: vi.fn(),
    listWebhookDeliveryAttempts: vi.fn(),
    replayWebhookDelivery: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showConfirmDestroy: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { default: InboundDetail } = await import('@/screens/inbound-detail.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const ID = '11111111111111111111111111111111';
const OUTBOUND = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
const DELIVERY = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb';
const SUB = '22222222222222222222222222222222';
const EVENT = 'e0000000000000000000000000000001';
const app = { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z' };

function detail(overrides: Record<string, unknown> = {}, message: Record<string, unknown> = {}) {
    return {
        message: {
            id: ID,
            received_at: '2026-11-13T10:00:00Z',
            from: { address: 'alice@example.org' },
            recipient: 'reply@mail.example.com',
            recipient_count: 1,
            subject: 'Re: hello',
            enhanced_protection: false,
            size_bytes: 2048,
            spf: 'pass',
            dkim: 'pass',
            dmarc: 'pass',
            correlation: { matched: false },
            webhook: { status: 'failed', total: 1, delivered: 0, pending: 0, failed: 1 },
            content_expired: false,
            ...message
        },
        content_expires_at: '2026-12-13T10:00:00Z',
        message_id_header: '<m@example.org>',
        envelope: { mail_from: 'bounce@example.org', recipients: ['reply@mail.example.com'], client_ip: '192.0.2.7', helo: 'mx.example.org', tls_version: 'TLS 1.3', tls_cipher: 'TLS_AES_128_GCM_SHA256' },
        authentication: {
            spf: { result: 'pass', domain: 'example.org', identity: 'mailfrom' },
            dkim: [{ result: 'fail', domain: 'example.org' }],
            dmarc: { result: 'pass', domain: 'example.org', policy: 'reject' },
            arc: { present: true, evaluated: false }
        },
        correlation: { matched: false },
        ...overrides
    };
}

const CONTENT = { from: 'José <alice@example.org>', to: ['reply@mail.example.com'], subject: 'Re: hello', text: 'Thanks', html: '<p>Thanks</p>', enhanced_protection: false, attachments: [] };
const FAILED_DELIVERY = { event_id: EVENT, subscription_id: SUB, inbound_id: ID, outcome: 'inbound_received', state: 'failed', attempts: 12, occurred_at: '2026-11-13T10:00:00Z' };

let wrapper: VueWrapper | undefined;

async function mountPage(id = ID) {
    const mounted = await mountAppRoute(`/apps/${APP_ID}/inbound/${id}`, { 'app-inbound-message': InboundDetail });
    wrapper = mounted.wrapper;
    return mounted;
}
const text = () => wrapper!.text();

const createObjectURL = vi.fn();
const revokeObjectURL = vi.fn();
let clickSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    authState.status = 'signed-in';
    api.listApps.mockResolvedValue({ data: { apps: [app] } });
    api.getInboundMessage.mockResolvedValue({ data: detail() });
    api.getInboundMessageContent.mockResolvedValue({ data: CONTENT });
    api.listInboundMessageWebhookDeliveries.mockResolvedValue({ data: { deliveries: [FAILED_DELIVERY] } });
    api.listAppWebhooks.mockResolvedValue({ data: { webhooks: [{ id: SUB, format: 'thinpost_inbound', endpoint: 'https://hooks.example.com/in', description: '', active: true, created_at: '2026-10-02T12:00:00Z' }] } });
    createObjectURL.mockReset().mockReturnValue('blob:eml');
    revokeObjectURL.mockReset();
    (URL as unknown as { createObjectURL: unknown }).createObjectURL = createObjectURL;
    (URL as unknown as { revokeObjectURL: unknown }).revokeObjectURL = revokeObjectURL;
    clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
    clickSpy.mockRestore();
});

describe('inbound message detail', () => {
    it('shows the message, how it arrived and the sender checks', async () => {
        await mountPage();
        expect(api.getInboundMessage).toHaveBeenCalledWith({ path: { appId: APP_ID, inboundId: ID } });
        expect(wrapper!.get('[data-fact="subject"]').text()).toContain('Re: hello');
        expect(wrapper!.get('[data-fact="from"]').text()).toContain('alice@example.org');
        expect(wrapper!.get('[data-fact="message-id-header"]').text()).toContain('<m@example.org>');
        const envelope = wrapper!.get('[data-testid="envelope"]').text();
        expect(envelope).toContain('bounce@example.org');
        expect(envelope).toContain('192.0.2.7');
        expect(envelope).toContain('mx.example.org');
        expect(envelope).toContain('TLS 1.3');
        const rows = wrapper!.findAll('[data-testid="authentication"] tbody tr');
        const check = (name: string) => rows.filter(r => r.attributes('data-check') === name).map(r => r.text());
        expect(check('spf')[0]).toContain('Pass');
        expect(check('spf')[0]).toContain("return address");
        expect(check('dkim')[0]).toContain('Fail');
        expect(check('dmarc')[0]).toContain('reject');
        // ARC is never evaluated, and the page says so even when ARC headers are present.
        expect(check('arc')[0]).toContain('Not evaluated');
        expect(check('arc')[0]).toContain('ARC headers');
        expect(wrapper!.find('[data-testid="no-correlation"]').exists()).toBe(true);
    });

    it('links a reply to the outbound message it answers', async () => {
        api.getInboundMessage.mockResolvedValue({
            data: detail({ correlation: { matched: true, message_id: OUTBOUND, delivery_id: DELIVERY, header: 'in_reply_to', from_matches_recipient: false } }, { correlation: { matched: true, message_id: OUTBOUND } })
        });
        await mountPage();
        const section = wrapper!.get('[data-testid="correlation"]');
        expect(section.get('a').attributes('href')).toBe(`/apps/${APP_ID}/messages/${OUTBOUND}`);
        expect(section.text()).toContain(DELIVERY);
        expect(section.text()).toContain('In-Reply-To');
        expect(section.text()).toContain('No');
        expect(text()).not.toContain('This doesn\'t answer a message');
    });

    it('loads the content through the inbound endpoint, in an isolated preview', async () => {
        await mountPage();
        expect(api.getInboundMessageContent).toHaveBeenCalledWith({ path: { appId: APP_ID, inboundId: ID } });
        expect(api.getInboundMessageContent).toHaveBeenCalledTimes(1);
        expect(text()).toContain('This is the message as it arrived.');
        expect(text()).toContain('José <alice@example.org>');
        const frame = wrapper!.get('iframe');
        expect(frame.element.getAttribute('sandbox')).toBe('');
    });

    it('lists the webhook events of this message and links nothing to itself', async () => {
        await mountPage();
        expect(api.listInboundMessageWebhookDeliveries).toHaveBeenCalledWith({ path: { appId: APP_ID, inboundId: ID }, query: { limit: 50 } });
        const history = wrapper!.get('section[aria-labelledby="deliveries-heading"]');
        expect(history.text()).toContain('inbound_received');
        expect(history.text()).toContain(`Inbound message ${ID}`);
        expect(history.find('a').exists()).toBe(false);
        expect(history.findAll('button').some(b => b.text().includes('Send again'))).toBe(true);
        expect(api.listAppWebhooks).toHaveBeenCalled();
    });

    it('downloads the .eml through an authenticated fetch', async () => {
        api.downloadInboundMessageRaw.mockResolvedValue({ data: new Blob(['From: a@b\r\n\r\nx']) });
        await mountPage();
        await wrapper!.get('[data-testid="download-eml"]').trigger('click');
        await flushPromises();
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(api.downloadInboundMessageRaw).toHaveBeenCalledWith({ path: { appId: APP_ID, inboundId: ID }, parseAs: 'blob' });
        expect(clickSpy).toHaveBeenCalledTimes(1);
        expect(location.href).not.toContain(ID);
    });

    it('shows the expired state instead of content when retention has ended', async () => {
        api.getInboundMessage.mockResolvedValue({ data: detail({}, { content_expired: true }) });
        await mountPage();
        expect(wrapper!.get('[data-testid="expired-tag"]').text()).toBe('Expired');
        expect(wrapper!.get('[data-testid="content-expired"]').text()).toContain('Expired (retention)');
        expect(api.getInboundMessageContent).not.toHaveBeenCalled();
        expect(wrapper!.get('[data-testid="download-eml"]').attributes('disabled')).toBeDefined();
        // The webhook history is still there: its events stay, they just can't be sent again.
        expect(api.listInboundMessageWebhookDeliveries).toHaveBeenCalled();
    });

    it('switches to the expired state when the server refuses the content with 409', async () => {
        api.getInboundMessageContent.mockRejectedValue(httpError(409));
        await mountPage();
        expect(wrapper!.find('[data-testid="content-expired"]').exists()).toBe(true);
        expect(wrapper!.find('[data-testid="expired-tag"]').exists()).toBe(true);
    });

    it('explains a refused download and expires the page on 409', async () => {
        api.downloadInboundMessageRaw.mockRejectedValue(httpError(409));
        await mountPage();
        await wrapper!.get('[data-testid="download-eml"]').trigger('click');
        await flushPromises();
        expect(text()).toContain('Expired (retention)');
        expect(createObjectURL).not.toHaveBeenCalled();
        expect(wrapper!.find('[data-testid="content-expired"]').exists()).toBe(true);
    });

    it('asks before showing or downloading an Enhanced Protection message, and masks its subject', async () => {
        api.getInboundMessage.mockResolvedValue({ data: detail({}, { enhanced_protection: true, subject: undefined }) });
        api.downloadInboundMessageRaw.mockResolvedValue({ data: new Blob(['x']) });
        vf.showConfirm.mockResolvedValue(true);
        await mountPage();
        expect(wrapper!.get('[data-fact="subject"]').text()).toContain('Protected');
        // Nothing is read, and nothing audited, until the operator asks.
        expect(api.getInboundMessageContent).not.toHaveBeenCalled();
        const show = wrapper!.findAll('button').find(b => b.text().includes('Show content'))!;
        await show.trigger('click');
        await flushPromises();
        expect(vf.showConfirm).toHaveBeenCalledTimes(1);
        expect(api.getInboundMessageContent).toHaveBeenCalledTimes(1);
        await wrapper!.get('[data-testid="download-eml"]').trigger('click');
        await flushPromises();
        expect(vf.showConfirm).toHaveBeenCalledTimes(2);
        expect(vf.showConfirm.mock.calls[1]![0]).toBe('Download protected message?');
        expect(api.downloadInboundMessageRaw).toHaveBeenCalledTimes(1);
    });

    it('does not download when the confirmation is declined', async () => {
        api.getInboundMessage.mockResolvedValue({ data: detail({}, { enhanced_protection: true }) });
        vf.showConfirm.mockResolvedValue(false);
        await mountPage();
        await wrapper!.get('[data-testid="download-eml"]').trigger('click');
        await flushPromises();
        expect(api.downloadInboundMessageRaw).not.toHaveBeenCalled();
    });

    it('says not found for another app\'s message and for a malformed ID, without reading anything', async () => {
        api.getInboundMessage.mockRejectedValue(httpError(404));
        await mountPage();
        expect(text()).toContain('Message not found');
        expect(api.getInboundMessageContent).not.toHaveBeenCalled();
        wrapper!.unmount();
        api.getInboundMessage.mockClear();
        await mountPage('nope');
        expect(text()).toContain('Message not found');
        expect(api.getInboundMessage).not.toHaveBeenCalled();
    });

    it('reports a response that does not match the contract', async () => {
        api.getInboundMessage.mockResolvedValue({ data: { message: { id: ID } } });
        await mountPage();
        expect(text()).toContain("didn't understand");
    });

    it('puts the received message in the breadcrumb', async () => {
        await mountPage();
        expect(wrapper!.get('nav[aria-label="Breadcrumb"]').text()).toContain('Inbound message');
        expect(wrapper!.find('nav[aria-label="Breadcrumb"] a[href$="/inbound"]').exists()).toBe(true);
    });
});
