import { InvalidResponseError } from '@/api-errors';
import { DefaultApi } from '@/openapi-client-generated';

/** How long the object URL lives after the click, as for attachment downloads, so every browser has started the save. */
export const OBJECT_URL_LIFETIME_MS = 30_000;

/**
 * Fetches a received message as an .eml file (authenticated, like every other call) and saves it through a
 * transient anchor. The bytes are labeled `application/octet-stream`, so the browser can never render them in the
 * console origin. Nothing goes in the address bar or browser storage: the object URL exists only in memory and is
 * revoked 30 seconds after the click, and the caller also gets `revoke` to cancel it early (leaving the page).
 * Resolves false when `shouldProceed` said the page moved on, so nothing was saved.
 */
export async function downloadInboundMessage(params: {
    appId: string;
    inboundId: string;
    /** Hands the pending revoke to the caller, which calls it on unmount. */
    onUrl?(revoke: () => void): void;
    /** Checked after the fetch; return false when the page moved on. */
    shouldProceed?: () => boolean;
}): Promise<boolean> {
    const { appId, inboundId } = params;
    const response = await DefaultApi.downloadInboundMessageRaw({ path: { appId, inboundId }, parseAs: 'blob' });
    const blob = response.data;
    if (params.shouldProceed && !params.shouldProceed()) return false;
    if (!(blob instanceof Blob)) throw new InvalidResponseError();
    const url = URL.createObjectURL(new Blob([blob], { type: 'application/octet-stream' }));
    let revoked = false;
    const revoke = () => {
        if (revoked) return;
        revoked = true;
        clearTimeout(timer);
        URL.revokeObjectURL(url);
    };
    const timer = setTimeout(revoke, OBJECT_URL_LIFETIME_MS);
    params.onUrl?.(revoke);
    const link = document.createElement('a');
    link.href = url;
    link.download = `inbound-${inboundId}.eml`;
    link.rel = 'noopener';
    document.body.appendChild(link);
    link.click();
    link.remove();
    return true;
}
