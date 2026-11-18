import { showConfirm } from '@zyno-io/vue-foundation';

import type { ApiErrorDescription } from '@/api-errors';

export type ProtectedAccess = 'subject' | 'content' | 'attachment' | 'headers' | 'raw';

const COPY: Record<ProtectedAccess, { title: string; message: string }> = {
    subject: {
        title: 'Reveal protected subject?',
        message:
            'This message uses Enhanced Protection, so its subject is hidden in lists and search. Revealing it shows it on this page only. The reveal is recorded in the audit log under your name.'
    },
    content: {
        title: 'View protected message content?',
        message:
            'This message uses Enhanced Protection. Viewing its addresses, subject, contents and inline images is recorded in the audit log under your name.'
    },
    attachment: {
        title: 'Download protected attachment?',
        message: 'This message uses Enhanced Protection. Downloading this attachment is recorded in the audit log under your name.'
    },
    raw: {
        title: 'Download protected message?',
        message:
            'This message uses Enhanced Protection. The downloaded file holds the whole message, including its subject and addresses. Downloading it is recorded in the audit log under your name.'
    },
    headers: {
        title: 'View protected delivery headers?',
        message:
            'This message uses Enhanced Protection. Its delivery headers include the subject and the recipient address. Viewing them is recorded in the audit log under your name.'
    }
};

/**
 * Asks the user to confirm an audited read. Only Enhanced Protection messages are audited, so any
 * other message resolves true without a dialog.
 */
export async function confirmProtectedAccess(kind: ProtectedAccess, options: { enhancedProtection: boolean }): Promise<boolean> {
    if (!options.enhancedProtection) return true;
    const copy = COPY[kind];
    const confirmed = await showConfirm(copy.title, copy.message);
    return confirmed === true;
}

export const AUDIT_FAILURE_SUFFIX = ' The attempt may still have been recorded in the audit log.';

/** Adds the audit note to a failure description; 401 and 403 are rejected before anything is read. */
export function withAuditSuffix(description: ApiErrorDescription): ApiErrorDescription {
    if (description.status === 401 || description.status === 403) return description;
    return { ...description, message: `${description.message}${AUDIT_FAILURE_SUFFIX}` };
}
