import { describe, expect, it } from 'vitest';

import { APP_SECTIONS } from '@/app-sections';

describe('app sections', () => {
    it('puts SMTP right after API keys', () => {
        expect(APP_SECTIONS.map(s => s.id)).toEqual(['keys', 'smtp', 'domains', 'messages', 'webhooks', 'tracking', 'suppressions', 'inbound']);
    });
});
