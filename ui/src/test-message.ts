import { apiErrorCode, apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription } from './api-errors';
import { isValidAddress } from './suppressions';

export { parseAddresses } from './suppressions';

/** The operator test-send profile allows 1 to 5 To recipients. */
export const MAX_TEST_RECIPIENTS = 5;

export const DEFAULT_TEST_SUBJECT = 'Test message from Thinpost';
export const DEFAULT_TEST_TEXT = 'This is a test message sent from the Thinpost console to check that delivery works. You can ignore it.';

export const MAX_SUBJECT_BYTES = 998;
export const MAX_LOCAL_PART_BYTES = 64;
export const MAX_DISPLAY_NAME_BYTES = 128;

const encoder = new TextEncoder();
// eslint-disable-next-line no-control-regex -- C0 controls and DEL
const CONTROL = /[\u0000-\u001f\u007f]/;
const ATOM_NAME = /^[A-Za-z0-9 !#$%&'*+\-/=?^_`{|}~]+$/;

/** Null when the local part can go in front of `@domain`; otherwise a short reason. The server stays authoritative. */
export function validateLocalPart(local: string): string | null {
    if (!local) return 'Enter the part before the @.';
    if (encoder.encode(local).length > MAX_LOCAL_PART_BYTES) return `Use at most ${MAX_LOCAL_PART_BYTES} bytes.`;
    if (!isValidAddress(`${local}@example.com`)) return 'Use letters, digits and simple punctuation, without spaces or @.';
    return null;
}

/** Null when the display name is acceptable (an empty name is). */
export function validateDisplayName(name: string): string | null {
    if (CONTROL.test(name)) return 'Remove control characters.';
    if (encoder.encode(name).length > MAX_DISPLAY_NAME_BYTES) return `Use at most ${MAX_DISPLAY_NAME_BYTES} bytes.`;
    return null;
}

/** Null when the subject fits the API's limits. */
export function validateSubject(subject: string): string | null {
    if (!subject.trim()) return 'Enter a subject.';
    // Tab is allowed; every other control character is refused.
    // eslint-disable-next-line no-control-regex -- C0 controls other than tab, and DEL
    if (/[\u0000-\u0008\u000a-\u001f\u007f]/.test(subject)) return 'Remove line breaks and control characters.';
    if (encoder.encode(subject).length > MAX_SUBJECT_BYTES) return `Use at most ${MAX_SUBJECT_BYTES} bytes.`;
    return null;
}

/**
 * Builds the `from` string: `local@domain`, or `Name <local@domain>` with the name quoted (RFC 5322
 * quoted-string) unless it is made only of atom characters and spaces.
 */
export function formatFrom(name: string, local: string, domain: string): string {
    const address = `${local}@${domain}`;
    const display = name.trim();
    if (!display) return address;
    const quoted = ATOM_NAME.test(display) ? display : `"${display.replace(/[\\"]/g, '\\$&')}"`;
    return `${quoted} <${address}>`;
}

/** Why the recipients cannot be sent yet, or null. */
export function recipientProblem(valid: number, invalid: number): string | null {
    if (invalid > 0) return `${invalid} invalid ${invalid === 1 ? 'address' : 'addresses'}.`;
    if (valid > MAX_TEST_RECIPIENTS) return `Use at most ${MAX_TEST_RECIPIENTS} recipients.`;
    return null;
}

/** True when no response settles whether the message was queued: no response, a 5xx, or an unusable 2xx. */
export function isUnknownOutcome(err: unknown): boolean {
    if (err instanceof InvalidResponseError) return true;
    const status = apiErrorStatus(err);
    return status === undefined || status >= 500;
}

const NOT_SENT = ' Nothing was sent.';

/** Copy for a definitive failure of a test send. Check `isUnknownOutcome` first. */
export function describeTestRefusal(err: unknown): ApiErrorDescription {
    const status = apiErrorStatus(err);
    switch (apiErrorCode(err)) {
        case 'app_paused':
            return { kind: 'conflict', status, message: `This app is paused. Resume it, then try again.${NOT_SENT}` };
        case 'message_rejected':
            return {
                kind: 'invalid',
                status,
                message: `Thinpost refused the message.${NOT_SENT} Common causes: the From domain isn't a verified sending domain of this app, or a recipient isn't allowed by the recipient rules.`
            };
        case 'submission_rate_exceeded':
            return {
                kind: 'unavailable',
                status,
                message: `This app is over its sending limit, or has none set. A new app can't send until a limit is set. Set one in the Sending limit panel on the API keys tab, then try again.${NOT_SENT}`
            };
        case 'recipient_quota_exceeded':
            return {
                kind: 'unavailable',
                status,
                message: `This app has reached its daily recipient limit, or none is set. Set one in the Daily recipient limit panel on the API keys tab, then try again.${NOT_SENT}`
            };
        case 'pending_limit_exceeded':
            return {
                kind: 'unavailable',
                status,
                message: `This app has reached its pending-message limit, or none is set. Wait for queued messages to finish, or raise it in the Pending-message limit panel on the API keys tab, then try again.${NOT_SENT}`
            };
    }
    return describeApiError(err, {
        notFound: `The app was not found, or this Thinpost server doesn't support test messages yet (for example during an upgrade).${NOT_SENT}`
    });
}
