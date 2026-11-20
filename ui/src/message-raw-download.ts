import { InvalidResponseError } from '@/api-errors';
import { DefaultApi } from '@/openapi-client-generated';

/**
 * Fetches the original message of a raw MIME submission and saves it as an .eml through a transient anchor. The
 * bytes are always labelled `application/octet-stream`, so the browser can never render them in the console
 * origin. The caller owns the object URL and must revoke it (`register` hands it over). Never navigates or
 * opens a window.
 */
export async function downloadMessageRaw(params: {
    appId: string;
    messageId: string;
    register(url: string): void;
    /** Checked after the fetch; return false when the page moved on, so nothing is saved. */
    shouldProceed?: () => boolean;
}): Promise<void> {
    const { appId, messageId } = params;
    const response = await DefaultApi.downloadMessageRaw({ path: { appId, messageId }, parseAs: 'blob' });
    const blob = response.data;
    if (params.shouldProceed && !params.shouldProceed()) return;
    if (!(blob instanceof Blob)) throw new InvalidResponseError();
    const url = URL.createObjectURL(new Blob([blob], { type: 'application/octet-stream' }));
    params.register(url);
    const link = document.createElement('a');
    link.href = url;
    link.download = `message-${messageId}.eml`;
    link.rel = 'noopener';
    document.body.appendChild(link);
    link.click();
    link.remove();
}
