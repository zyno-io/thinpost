import { describe, expect, it } from 'vitest';

import { readMessageBody } from '@/message-body';

function raw(overrides: Record<string, unknown> = {}) {
    return {
        from: 'a@example.com',
        to: ['b@example.net'],
        subject: 'Hi',
        text: 'hello',
        html: '<b>hello</b>',
        enhanced_protection: false,
        attachments: [{ index: 0, filename: 'a.png', content_type: 'image/png', size: 10, inline: true, content_id: 'x' }],
        ...overrides
    };
}

describe('readMessageBody', () => {
    it('reads a valid body and drops unknown fields', () => {
        const body = readMessageBody(raw({ extra: 1, reply_to: ['r@example.com'] }));
        expect(body).toMatchObject({ from: 'a@example.com', replyTo: ['r@example.com'], enhancedProtection: false });
        expect(body?.attachments[0]).toEqual({ index: 0, filename: 'a.png', contentType: 'image/png', size: 10, inline: true, contentId: 'x' });
        expect(Object.keys(body!)).not.toContain('extra');
    });
    it('reads the raw flag, defaulting to false, and rejects a non-boolean', () => {
        expect(readMessageBody(raw())?.raw).toBe(false);
        expect(readMessageBody(raw({ raw: true }))?.raw).toBe(true);
        expect(readMessageBody(raw({ raw: 'yes' }))).toBeNull();
    });
    it('reads Cc and Bcc, defaulting to none, and rejects malformed lists', () => {
        expect(readMessageBody(raw())).toMatchObject({ cc: [], bcc: [] });
        expect(readMessageBody(raw({ cc: ['c@example.com'], bcc: ['d@example.com'] }))).toMatchObject({ cc: ['c@example.com'], bcc: ['d@example.com'] });
        expect(readMessageBody(raw({ cc: [1] }))).toBeNull();
        expect(readMessageBody(raw({ bcc: 'd@example.com' }))).toBeNull();
    });
    it('rejects bad attachments', () => {
        const att = { index: 0, filename: 'a', content_type: 't', size: 1, inline: false };
        expect(readMessageBody(raw({ attachments: [{ ...att, index: 20 }] }))).toBeNull();
        expect(readMessageBody(raw({ attachments: [{ ...att, index: -1 }] }))).toBeNull();
        expect(readMessageBody(raw({ attachments: [att, att] }))).toBeNull();
        const { inline: _inline, ...noInline } = att;
        expect(readMessageBody(raw({ attachments: [noInline] }))).toBeNull();
        expect(readMessageBody(raw({ attachments: [{ ...att, size: -1 }] }))).toBeNull();
    });
    it('rejects missing fields', () => {
        expect(readMessageBody(raw({ enhanced_protection: undefined }))).toBeNull();
        expect(readMessageBody(raw({ attachments: undefined }))).toBeNull();
        expect(readMessageBody(raw({ to: [1] }))).toBeNull();
        expect(readMessageBody(null)).toBeNull();
    });
});
