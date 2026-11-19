import { describe, expect, it } from 'vitest';

import { messageStatus } from '@/states';

describe('messageStatus', () => {
    it.each([
        ['sending', 'Sending', 'inactive'],
        ['retrying', 'Retrying', 'warning'],
        ['delivered', 'Delivered', 'active'],
        ['partially_delivered', 'Partially delivered', 'warning'],
        ['failed', 'Failed', 'danger'],
        ['suppressed', 'Suppressed', 'inactive'],
        ['attention', 'Needs attention', 'warning'],
        ['on_hold', 'On hold by app', 'inactive'],
        ['cancelled', 'Cancelled', 'inactive'],
        ['expired', 'Hold expired', 'inactive']
    ])('labels %s', (status, label, tone) => {
        expect(messageStatus(status)).toMatchObject({ label, tone });
    });

    it('shows an unrecognized status verbatim', () => {
        expect(messageStatus('mystery')).toEqual({ label: 'mystery', tone: 'inactive' });
    });
});
