import { describe, expect, it } from 'vitest';

import { readMessageIdHeader } from '@/message-id-header';

const ID = '0123456789abcdef0123456789abcdef';
const OTHER = 'fedcba9876543210fedcba9876543210';

describe('readMessageIdHeader', () => {
    it('reports the shared header when every delivery carries the message-scoped one', () => {
        const value = `<${ID}@example.com>`;
        expect(readMessageIdHeader(ID, 'prepared', [{ message_id_header: value }, { message_id_header: value }])).toEqual({ kind: 'shared', value });
    });

    it('reports per recipient for a delivery-scoped header', () => {
        expect(readMessageIdHeader(ID, 'prepared', [{ message_id_header: `<${OTHER}@example.com>` }, { message_id_header: `<${ID}@example.com>` }])).toEqual({ kind: 'perRecipient' });
    });

    it('is pending before any delivery exists, and unrecorded otherwise without a value', () => {
        for (const state of ['queued', 'preparing', 'preparation_failed', 'on_hold']) expect(readMessageIdHeader(ID, state, [])).toEqual({ kind: 'pending' });
        expect(readMessageIdHeader(ID, 'prepared', [{}, {}])).toEqual({ kind: 'unrecorded' });
        expect(readMessageIdHeader(ID, 'prepared', [])).toEqual({ kind: 'unrecorded' });
    });

    it('is never assigned for a hold that ended without sending', () => {
        for (const state of ['cancelled', 'hold_expired']) expect(readMessageIdHeader(ID, state, [])).toEqual({ kind: 'notSent' });
    });

    it('ignores malformed values and never displays them', () => {
        for (const bad of [`${ID}@example.com`, `<${ID}@Example.com>`, `<${ID.toUpperCase()}@example.com>`, `<${ID}@>`, `<${ID}@a b>`, `<${ID}@example.com> `, '<x@y>', 42, null, {}]) {
            expect(readMessageIdHeader(ID, 'prepared', [{ message_id_header: bad }])).toEqual({ kind: 'unrecorded' });
        }
    });
});
