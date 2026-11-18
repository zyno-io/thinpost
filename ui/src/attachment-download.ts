import { InvalidResponseError } from '@/api-errors';
import { DefaultApi } from '@/openapi-client-generated';
import type { AttachmentInfo } from '@/message-body';

// eslint-disable-next-line no-control-regex -- control characters, path separators, Windows-reserved characters and bidi overrides
const UNSAFE_FILENAME_CHARS = /[\u0000-\u001f\u007f/\\:*?"<>|‪-‮⁦-⁩]/g;
const MAX_FILENAME_LENGTH = 180;

/** A filename that is safe to hand to the browser's download UI. */
export function safeFilename(name: string, index: number): string {
    const cleaned = name.replace(UNSAFE_FILENAME_CHARS, '_').trim().replace(/^\.+/, '').trim().slice(0, MAX_FILENAME_LENGTH).trim();
    return cleaned || `attachment-${index + 1}`;
}

/**
 * Fetches an attachment and saves it through a transient anchor. The bytes are always labelled
 * `application/octet-stream`, so the browser can never render them in the console origin. The caller owns
 * the object URL and must revoke it (`register` hands it over). Never navigates or opens a window.
 */
export async function downloadAttachment(params: {
    appId: string;
    messageId: string;
    attachment: AttachmentInfo;
    register(url: string): void;
    /** Checked after the fetch; return false when the page moved on, so nothing is saved. */
    shouldProceed?: () => boolean;
}): Promise<void> {
    const { appId, messageId, attachment } = params;
    const response = await DefaultApi.downloadAppMessageAttachment({ path: { appId, messageId, index: attachment.index }, parseAs: 'blob' });
    const blob = response.data;
    if (params.shouldProceed && !params.shouldProceed()) return;
    if (!(blob instanceof Blob)) throw new InvalidResponseError();
    const file = new Blob([blob], { type: 'application/octet-stream' });
    const url = URL.createObjectURL(file);
    params.register(url);
    const link = document.createElement('a');
    link.href = url;
    link.download = safeFilename(attachment.filename, attachment.index);
    link.rel = 'noopener';
    document.body.appendChild(link);
    link.click();
    link.remove();
}
