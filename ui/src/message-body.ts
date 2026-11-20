export interface AttachmentInfo {
    index: number;
    filename: string;
    contentType: string;
    size: number;
    inline: boolean;
    contentId?: string;
}

export interface ParsedMessageBody {
    from: string;
    replyTo: string[];
    to: string[];
    cc: string[];
    bcc: string[];
    subject: string;
    text: string;
    html: string;
    enhancedProtection: boolean;
    attachments: AttachmentInfo[];
    /** The message was submitted as raw MIME over SMTP, so its original can be downloaded as an .eml. */
    raw: boolean;
    /** Inbound reads only: what could not be shown (`text`, `html` or `headers`). Empty for outbound messages. */
    omitted: string[];
}

export const MAX_ATTACHMENT_INDEX = 19;

function isStringArray(value: unknown): value is string[] {
    return Array.isArray(value) && value.every(item => typeof item === 'string');
}

function readAttachment(raw: unknown): AttachmentInfo | null {
    if (!raw || typeof raw !== 'object') return null;
    const v = raw as Record<string, unknown>;
    if (
        typeof v.index !== 'number' ||
        !Number.isInteger(v.index) ||
        v.index < 0 ||
        v.index > MAX_ATTACHMENT_INDEX ||
        typeof v.filename !== 'string' ||
        typeof v.content_type !== 'string' ||
        typeof v.size !== 'number' ||
        !Number.isSafeInteger(v.size) ||
        v.size < 0 ||
        typeof v.inline !== 'boolean' ||
        (v.content_id !== undefined && typeof v.content_id !== 'string')
    ) {
        return null;
    }
    const info: AttachmentInfo = { index: v.index, filename: v.filename, contentType: v.content_type, size: v.size, inline: v.inline };
    if (typeof v.content_id === 'string' && v.content_id) info.contentId = v.content_id;
    return info;
}

/** Validates a message body response and keeps only contract fields; null when it does not match. */
export function readMessageBody(raw: unknown): ParsedMessageBody | null {
    if (!raw || typeof raw !== 'object') return null;
    const v = raw as Record<string, unknown>;
    if (
        typeof v.from !== 'string' ||
        typeof v.subject !== 'string' ||
        typeof v.text !== 'string' ||
        typeof v.html !== 'string' ||
        typeof v.enhanced_protection !== 'boolean' ||
        !isStringArray(v.to) ||
        (v.reply_to !== undefined && !isStringArray(v.reply_to)) ||
        (v.cc !== undefined && !isStringArray(v.cc)) ||
        (v.bcc !== undefined && !isStringArray(v.bcc)) ||
        !Array.isArray(v.attachments) ||
        (v.omitted !== undefined && !isStringArray(v.omitted)) ||
        (v.raw !== undefined && typeof v.raw !== 'boolean')
    ) {
        return null;
    }
    const attachments: AttachmentInfo[] = [];
    const seen = new Set<number>();
    for (const item of v.attachments) {
        const attachment = readAttachment(item);
        if (!attachment || seen.has(attachment.index)) return null;
        seen.add(attachment.index);
        attachments.push(attachment);
    }
    return {
        from: v.from,
        replyTo: v.reply_to ?? [],
        to: v.to,
        cc: v.cc ?? [],
        bcc: v.bcc ?? [],
        subject: v.subject,
        text: v.text,
        html: v.html,
        enhancedProtection: v.enhanced_protection,
        attachments,
        raw: v.raw === true,
        omitted: v.omitted ?? []
    };
}
