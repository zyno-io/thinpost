import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, openHelp } from './helpers';

const api = vi.hoisted(() => ({
    getAppMessageBody: vi.fn(),
    downloadAppMessageAttachment: vi.fn(),
    getPublicConfig: vi.fn()
}));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const vf = vi.hoisted(() => ({ showConfirm: vi.fn(), showToast: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => {
    const original = await importOriginal<object>();
    return { ...original, ...vf };
});

const { authState } = await import('@/auth');
const { default: MessageContent } = await import('@/shared/components/message-content.vue');

const APP_ID = '0123456789abcdef0123456789abcdef';
const MESSAGE_ID = 'fedcba9876543210fedcba9876543210';
const HTML = '<img src=x onerror="window.__pwned=1"><script>window.__pwned=2</script><b>Hi</b><img src="https://t.test/p.png">';

function bodyOf(overrides: Record<string, unknown> = {}) {
    return {
        from: 'billing@example.com',
        to: ['ada@example.org', 'grace@example.net'],
        subject: 'Your invoice <b>',
        text: 'Hello\nworld',
        html: HTML,
        enhanced_protection: false,
        attachments: [],
        ...overrides
    };
}

let wrapper: VueWrapper | undefined;

function mountContent(enhancedProtection = false) {
    wrapper = mount(MessageContent, { props: { appId: APP_ID, messageId: MESSAGE_ID, enhancedProtection }, attachTo: document.body });
    return wrapper;
}

function button(label: string) {
    const match = wrapper!.findAll('button').find(b => b.text().includes(label));
    if (!match) throw new Error(`button "${label}" not found`);
    return match;
}

const createObjectURL = vi.fn();
const revokeObjectURL = vi.fn();
let clickSpy: ReturnType<typeof vi.spyOn>;

beforeEach(() => {
    Object.values(api).forEach(fn => fn.mockReset());
    Object.values(vf).forEach(fn => fn.mockReset());
    createObjectURL.mockReset().mockReturnValue('blob:test-1');
    revokeObjectURL.mockReset();
    (URL as unknown as { createObjectURL: unknown }).createObjectURL = createObjectURL;
    (URL as unknown as { revokeObjectURL: unknown }).revokeObjectURL = revokeObjectURL;
    clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    authState.status = 'signed-in';
    delete (window as { __pwned?: unknown }).__pwned;
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
    vi.useRealTimers();
    clickSpy.mockRestore();
});

describe('message content (non-EP)', () => {
    beforeEach(() => {
        api.getAppMessageBody.mockResolvedValue({ data: bodyOf() });
    });

    it('loads on mount without a dialog and renders HTML only in a sandboxed iframe', async () => {
        mountContent();
        await flushPromises();
        expect(api.getAppMessageBody).toHaveBeenCalledWith({ path: { appId: APP_ID, messageId: MESSAGE_ID } });
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(button('Hide content')).toBeTruthy();
        expect(wrapper!.find('[role=tab][aria-selected=true]').text()).toBe('Rendered');

        const frames = wrapper!.findAll('iframe');
        expect(frames).toHaveLength(1);
        const frame = frames[0]!;
        expect(frame.element.getAttribute('sandbox')).toBe('');
        expect(frame.attributes('referrerpolicy')).toBe('no-referrer');
        const srcdoc = frame.attributes('srcdoc')!;
        expect(srcdoc).toContain("default-src 'none'");
        expect(srcdoc).not.toContain('<script');
        expect(srcdoc).not.toContain('onerror=');
        expect(srcdoc).toContain('<b>Hi</b>');

        // Email markup never enters the console DOM.
        expect(wrapper!.element.querySelector('img, script, b')).toBeNull();
        expect((window as { __pwned?: unknown }).__pwned).toBeUndefined();
        expect(wrapper!.text()).not.toMatch(/audit/i);
        expect(wrapper!.text()).not.toContain('Images and other content from the internet are never loaded here');
        expect(await openHelp(wrapper!, 'message content')).toContain('Images and other content from the internet are never loaded here');
    });

    it('shows the text and HTML source tabs as plain text', async () => {
        mountContent();
        await flushPromises();
        await wrapper!.findAll('[role=tab]')[1]!.trigger('click');
        expect(wrapper!.find('pre[aria-label="Text part"]').text()).toBe('Hello\nworld');
        expect(wrapper!.find('iframe').exists()).toBe(false);
        await wrapper!.findAll('[role=tab]')[2]!.trigger('click');
        const source = wrapper!.find('pre[aria-label="HTML part source"]');
        expect(source.text()).toBe(HTML);
        expect(source.element.children).toHaveLength(0);
    });

    it('moves between tabs with the arrow keys', async () => {
        mountContent();
        await flushPromises();
        await wrapper!.find('[role=tablist]').trigger('keydown', { key: 'ArrowRight' });
        expect(wrapper!.find('[role=tab][aria-selected=true]').text()).toBe('Text');
        await wrapper!.find('[role=tablist]').trigger('keydown', { key: 'End' });
        expect(wrapper!.find('[role=tab][aria-selected=true]').text()).toBe('HTML source');
        await wrapper!.find('[role=tablist]').trigger('keydown', { key: 'ArrowRight' });
        expect(wrapper!.find('[role=tab][aria-selected=true]').text()).toBe('Rendered');
    });

    it('states how many external resources were blocked and offers no remote-content toggle', async () => {
        mountContent();
        await flushPromises();
        expect(wrapper!.find('.blocked-note').text()).toContain('2 external items blocked');
        expect(wrapper!.find('input[type=checkbox]').exists()).toBe(false);
        expect(wrapper!.text()).not.toMatch(/load remote|show images/i);
    });

    it('uses the text tab when there is no HTML part', async () => {
        api.getAppMessageBody.mockResolvedValue({ data: bodyOf({ html: '' }) });
        mountContent();
        await flushPromises();
        expect(wrapper!.find('[role=tab][aria-selected=true]').text()).toBe('Text');
        expect(wrapper!.findAll('[role=tab]')[0]!.attributes('disabled')).toBeDefined();
    });

    it('discards the content on Hide and reloads without a dialog', async () => {
        mountContent();
        await flushPromises();
        await button('Hide content').trigger('click');
        expect(wrapper!.text()).not.toContain('billing@example.com');
        expect(wrapper!.find('iframe').exists()).toBe(false);
        await button('Show content').trigger('click');
        await flushPromises();
        expect(api.getAppMessageBody).toHaveBeenCalledTimes(2);
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('billing@example.com');
    });

    it('discards the content when the session ends and disables Show content', async () => {
        mountContent();
        await flushPromises();
        authState.status = 'ended';
        await flushPromises();
        expect(wrapper!.text()).not.toContain('billing@example.com');
        expect(wrapper!.find('iframe').exists()).toBe(false);
        expect(button('Show content').attributes('disabled')).toBeDefined();
    });

    it('drops content on unmount', async () => {
        mountContent();
        await flushPromises();
        expect(document.body.innerHTML).toContain('billing@example.com');
        wrapper!.unmount();
        wrapper = undefined;
        expect(document.body.innerHTML).not.toContain('billing@example.com');
    });

    it('drops a response that arrives after the session ended or after unmount', async () => {
        let resolve: (v: unknown) => void = () => {};
        api.getAppMessageBody.mockReturnValue(new Promise(r => (resolve = r)));
        mountContent();
        await flushPromises();
        authState.status = 'ended';
        await flushPromises();
        resolve({ data: bodyOf() });
        await flushPromises();
        expect(wrapper!.text()).not.toContain('billing@example.com');

        authState.status = 'signed-in';
        wrapper!.unmount();
        let resolve2: (v: unknown) => void = () => {};
        api.getAppMessageBody.mockReturnValue(new Promise(r => (resolve2 = r)));
        mountContent();
        await flushPromises();
        wrapper!.unmount();
        wrapper = undefined;
        resolve2({ data: bodyOf() });
        await flushPromises();
        expect(document.body.innerHTML).not.toContain('billing@example.com');
    });

    it('shows a malformed body as an error without content', async () => {
        api.getAppMessageBody.mockResolvedValue({ data: { from: 'x' } });
        mountContent();
        await flushPromises();
        expect(wrapper!.text()).toContain("Thinpost sent back something the console didn't understand");
        expect(wrapper!.find('pre').exists()).toBe(false);
        expect(wrapper!.find('iframe').exists()).toBe(false);
    });

    it('reports a storage failure without any audit wording', async () => {
        api.getAppMessageBody.mockRejectedValue(httpError(503));
        mountContent();
        await flushPromises();
        expect(wrapper!.text()).toContain("The message couldn't be read right now");
        expect(wrapper!.text()).not.toMatch(/audit/i);
    });

    it('falls back to source for oversize and unsafe HTML', async () => {
        api.getAppMessageBody.mockResolvedValue({ data: bodyOf({ html: 'x'.repeat(2_000_001) }) });
        mountContent();
        await flushPromises();
        expect(wrapper!.text()).toContain('too large to preview');
        expect(wrapper!.find('iframe').exists()).toBe(false);
    });
});

describe('message content (inline images)', () => {
    const png = new Blob([new Uint8Array([137, 80, 78, 71])], { type: 'application/octet-stream' });
    const attachment = (overrides: Record<string, unknown> = {}) => ({
        index: 0,
        filename: 'logo.png',
        content_type: 'image/png',
        size: 4,
        inline: true,
        content_id: '<Logo@X>',
        ...overrides
    });

    it('resolves cid images to data URLs within the caps', async () => {
        api.getAppMessageBody.mockResolvedValue({
            data: bodyOf({ html: '<img src="cid:logo@x">', attachments: [attachment()] })
        });
        api.downloadAppMessageAttachment.mockResolvedValue({ data: png });
        mountContent();
        await flushPromises();
        expect(api.downloadAppMessageAttachment).toHaveBeenCalledWith({ path: { appId: APP_ID, messageId: MESSAGE_ID, index: 0 }, parseAs: 'blob' });
        expect(wrapper!.find('iframe').attributes('srcdoc')).toContain('data:image/png;base64,');
    });

    it('does not fetch oversize, non-image or unreferenced attachments, and reports unresolved images', async () => {
        api.getAppMessageBody.mockResolvedValue({
            data: bodyOf({
                html: '<img src="cid:big"><img src="cid:doc">',
                attachments: [
                    attachment({ index: 0, content_id: 'big', size: 6 * 1024 * 1024 }),
                    attachment({ index: 1, content_id: 'doc', content_type: 'application/pdf' }),
                    attachment({ index: 2, content_id: 'unused' })
                ]
            })
        });
        mountContent();
        await flushPromises();
        expect(api.downloadAppMessageAttachment).not.toHaveBeenCalled();
        expect(wrapper!.find('.blocked-note').text()).toContain('2 inline images not shown');
    });

    it('counts a failed inline fetch as unresolved', async () => {
        api.getAppMessageBody.mockResolvedValue({ data: bodyOf({ html: '<img src="cid:logo@x">', attachments: [attachment()] }) });
        api.downloadAppMessageAttachment.mockRejectedValue(httpError(503));
        mountContent();
        await flushPromises();
        expect(wrapper!.find('.blocked-note').text()).toContain('1 inline image not shown');
    });
});

describe('message content (Enhanced Protection)', () => {
    beforeEach(() => {
        api.getAppMessageBody.mockResolvedValue({ data: bodyOf({ enhanced_protection: true }) });
    });

    it('fetches nothing until confirmed', async () => {
        mountContent(true);
        await flushPromises();
        expect(api.getAppMessageBody).not.toHaveBeenCalled();
        expect(await openHelp(wrapper!, 'message content')).toContain('recorded in the audit log under your name');

        vf.showConfirm.mockResolvedValueOnce(false);
        await button('Show content').trigger('click');
        await flushPromises();
        expect(vf.showConfirm.mock.calls[0]![1]).toContain('audit log');
        expect(api.getAppMessageBody).not.toHaveBeenCalled();

        vf.showConfirm.mockResolvedValueOnce(true);
        await button('Show content').trigger('click');
        await flushPromises();
        expect(api.getAppMessageBody).toHaveBeenCalledTimes(1);
        expect(wrapper!.text()).toContain('billing@example.com');
    });

    it('appends the audit suffix to failures', async () => {
        api.getAppMessageBody.mockRejectedValue(httpError(503));
        vf.showConfirm.mockResolvedValue(true);
        mountContent(true);
        await button('Show content').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("The message couldn't be read right now");
        expect(wrapper!.text()).toContain('may still have been recorded in the audit log');
    });

    it('does not fetch when the confirmation resolves after unmount', async () => {
        let resolve: (v: boolean) => void = () => {};
        vf.showConfirm.mockReturnValue(new Promise(r => (resolve = r)));
        mountContent(true);
        await button('Show content').trigger('click');
        wrapper!.unmount();
        wrapper = undefined;
        resolve(true);
        await flushPromises();
        expect(api.getAppMessageBody).not.toHaveBeenCalled();
    });
});

describe('attachment downloads', () => {
    const attachments = [{ index: 3, filename: '../../evil:name?.txt', content_type: 'text/plain', size: 2048, inline: false }];

    beforeEach(() => {
        api.getAppMessageBody.mockResolvedValue({ data: bodyOf({ attachments, html: '' }) });
        api.downloadAppMessageAttachment.mockResolvedValue({ data: new Blob(['hello'], { type: 'text/html' }) });
    });

    it('lists attachments and saves an octet-stream blob under a sanitized name', async () => {
        vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
        mountContent();
        await flushPromises();
        expect(wrapper!.find('.attachments').text()).toContain('2.0 KiB');
        let anchor: HTMLAnchorElement | undefined;
        clickSpy.mockImplementation(function (this: HTMLAnchorElement) {
            anchor = this;
        });
        await button('Download').trigger('click');
        await flushPromises();
        expect(api.downloadAppMessageAttachment).toHaveBeenCalledWith({ path: { appId: APP_ID, messageId: MESSAGE_ID, index: 3 }, parseAs: 'blob' });
        const blob = createObjectURL.mock.calls[0]![0] as Blob;
        expect(blob.type).toBe('application/octet-stream');
        expect(anchor!.download).toBe('_.._evil_name_.txt');
        expect(anchor!.getAttribute('href')).toBe('blob:test-1');
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(revokeObjectURL).not.toHaveBeenCalled();
        await vi.advanceTimersByTimeAsync(30_000);
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:test-1');
    });

    it('revokes outstanding object URLs on unmount', async () => {
        mountContent();
        await flushPromises();
        await button('Download').trigger('click');
        await flushPromises();
        wrapper!.unmount();
        wrapper = undefined;
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:test-1');
    });

    it('confirms first for Enhanced Protection and does nothing when declined', async () => {
        api.getAppMessageBody.mockResolvedValue({ data: bodyOf({ enhanced_protection: true, attachments, html: '' }) });
        vf.showConfirm.mockResolvedValueOnce(true).mockResolvedValueOnce(false);
        mountContent(true);
        await button('Show content').trigger('click');
        await flushPromises();
        await button('Download').trigger('click');
        await flushPromises();
        expect(vf.showConfirm).toHaveBeenCalledTimes(2);
        expect(api.downloadAppMessageAttachment).not.toHaveBeenCalled();
        expect(createObjectURL).not.toHaveBeenCalled();
    });

    it('reports download failures; non-EP without audit wording, EP with it', async () => {
        api.downloadAppMessageAttachment.mockRejectedValue(httpError(404));
        mountContent();
        await flushPromises();
        await button('Download').trigger('click');
        await flushPromises();
        expect(wrapper!.text()).toContain("That attachment wasn't found.");
        expect(wrapper!.text()).not.toMatch(/audit/i);
        wrapper!.unmount();

        api.getAppMessageBody.mockResolvedValue({ data: bodyOf({ enhanced_protection: true, attachments, html: '' }) });
        api.downloadAppMessageAttachment.mockRejectedValue(httpError(503));
        vf.showConfirm.mockResolvedValue(true);
        mountContent(true);
        await button('Show content').trigger('click');
        await flushPromises();
        await button('Download').trigger('click');
        await flushPromises();
        expect(wrapper!.find('.attachments-section').text()).toContain('may still have been recorded in the audit log');
    });
});

describe('message content with a loader (inbound mail)', () => {
    function mountReceived(loader: () => Promise<unknown>, errors?: Record<string, string>, enhancedProtection = false) {
        wrapper = mount(MessageContent, { props: { appId: APP_ID, messageId: MESSAGE_ID, enhancedProtection, loader, received: true, errors }, attachTo: document.body });
        return wrapper;
    }

    it('reads through the loader and never touches the outbound body endpoint', async () => {
        const loader = vi.fn().mockResolvedValue(bodyOf({ html: '', text: 'hi there' }));
        mountReceived(loader);
        await flushPromises();
        expect(loader).toHaveBeenCalledTimes(1);
        expect(api.getAppMessageBody).not.toHaveBeenCalled();
        expect(wrapper!.text()).toContain('This is the message as it arrived.');
        expect(wrapper!.text()).not.toContain('This is the content your app sent.');
        expect(wrapper!.find('pre[aria-label="Text part"]').text()).toBe('hi there');
    });

    it('validates what the loader returns', async () => {
        mountReceived(vi.fn().mockResolvedValue({ from: 1 }));
        await flushPromises();
        expect(wrapper!.text()).toContain("didn't understand");
    });

    it('uses the supplied wording for a refused read', async () => {
        mountReceived(vi.fn().mockRejectedValue(httpError(409)), { conflict: 'Expired (retention).' });
        await flushPromises();
        expect(wrapper!.text()).toContain('Expired (retention).');
    });

    it('says when part of the message could not be shown', async () => {
        mountReceived(vi.fn().mockResolvedValue(bodyOf({ html: '', text: '', omitted: ['text', 'html'] })));
        await flushPromises();
        expect(wrapper!.get('[data-testid="omitted-note"]').text()).toContain('the text and the HTML');
    });

    it('still asks before an Enhanced Protection read', async () => {
        const loader = vi.fn().mockResolvedValue(bodyOf({ enhanced_protection: true }));
        vf.showConfirm.mockResolvedValue(false);
        mountReceived(loader, undefined, true);
        await flushPromises();
        expect(loader).not.toHaveBeenCalled();
        vf.showConfirm.mockResolvedValue(true);
        await button('Show content').trigger('click');
        await flushPromises();
        expect(loader).toHaveBeenCalledTimes(1);
    });
});
