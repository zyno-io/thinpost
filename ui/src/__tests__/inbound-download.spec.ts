import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError } from './helpers';

const api = vi.hoisted(() => ({ downloadInboundMessageRaw: vi.fn() }));
vi.mock('@/openapi-client-generated', () => ({ DefaultApi: api }));

const { downloadInboundMessage, OBJECT_URL_LIFETIME_MS } = await import('@/inbound-download');

const APP_ID = '0123456789abcdef0123456789abcdef';
const ID = '11111111111111111111111111111111';

const createObjectURL = vi.fn();
const revokeObjectURL = vi.fn();
let clickSpy: ReturnType<typeof vi.spyOn>;
let linkAtClick: { href: string; download: string; rel: string } | null = null;

beforeEach(() => {
    vi.useFakeTimers();
    api.downloadInboundMessageRaw.mockReset();
    createObjectURL.mockReset().mockReturnValue('blob:eml-1');
    revokeObjectURL.mockReset();
    (URL as unknown as { createObjectURL: unknown }).createObjectURL = createObjectURL;
    (URL as unknown as { revokeObjectURL: unknown }).revokeObjectURL = revokeObjectURL;
    linkAtClick = null;
    clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
        linkAtClick = { href: this.href, download: this.download, rel: this.rel };
    });
});

afterEach(() => {
    vi.useRealTimers();
    clickSpy.mockRestore();
});

describe('downloadInboundMessage', () => {
    it('saves the file through an authenticated fetch and revokes the object URL after the click', async () => {
        api.downloadInboundMessageRaw.mockResolvedValue({ data: new Blob(['From: a@b\r\n\r\nhi'], { type: 'message/rfc822' }) });
        const done = await downloadInboundMessage({ appId: APP_ID, inboundId: ID });
        expect(done).toBe(true);
        expect(api.downloadInboundMessageRaw).toHaveBeenCalledWith({ path: { appId: APP_ID, inboundId: ID }, parseAs: 'blob' });
        // The bytes are relabeled so the browser can never render them in the console origin.
        expect((createObjectURL.mock.calls[0]![0] as Blob).type).toBe('application/octet-stream');
        expect(clickSpy).toHaveBeenCalledTimes(1);
        expect(linkAtClick).toMatchObject({ href: 'blob:eml-1', download: `inbound-${ID}.eml`, rel: 'noopener' });
        expect(document.querySelector('a[download]')).toBeNull();
        // Nothing in the URL or storage.
        expect(location.href).not.toContain(ID);
        expect(localStorage.length + sessionStorage.length).toBe(0);
        expect(revokeObjectURL).not.toHaveBeenCalled();
        vi.advanceTimersByTime(OBJECT_URL_LIFETIME_MS);
        expect(revokeObjectURL).toHaveBeenCalledWith('blob:eml-1');
    });

    it('lets the caller revoke early, once', async () => {
        api.downloadInboundMessageRaw.mockResolvedValue({ data: new Blob(['x']) });
        let revoke: (() => void) | undefined;
        await downloadInboundMessage({ appId: APP_ID, inboundId: ID, onUrl: r => (revoke = r) });
        revoke!();
        revoke!();
        vi.advanceTimersByTime(OBJECT_URL_LIFETIME_MS * 2);
        expect(revokeObjectURL).toHaveBeenCalledTimes(1);
    });

    it('saves nothing when the page moved on during the fetch', async () => {
        api.downloadInboundMessageRaw.mockResolvedValue({ data: new Blob(['x']) });
        expect(await downloadInboundMessage({ appId: APP_ID, inboundId: ID, shouldProceed: () => false })).toBe(false);
        expect(createObjectURL).not.toHaveBeenCalled();
        expect(clickSpy).not.toHaveBeenCalled();
    });

    it('rejects a body that is not a file and passes API errors through', async () => {
        api.downloadInboundMessageRaw.mockResolvedValue({ data: { nope: true } });
        await expect(downloadInboundMessage({ appId: APP_ID, inboundId: ID })).rejects.toThrow();
        expect(createObjectURL).not.toHaveBeenCalled();
        api.downloadInboundMessageRaw.mockRejectedValue(httpError(409));
        await expect(downloadInboundMessage({ appId: APP_ID, inboundId: ID })).rejects.toMatchObject({ response: { status: 409 } });
        expect(clickSpy).not.toHaveBeenCalled();
    });
});
