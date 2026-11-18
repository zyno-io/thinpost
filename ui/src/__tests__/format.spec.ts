import { describe, expect, it } from 'vitest';

import { formatRelativeTime } from '@/format';

const NOW = Date.parse('2026-11-10T12:00:00Z');
const at = (ms: number) => new Date(NOW + ms).toISOString();

describe('formatRelativeTime', () => {
    it('formats future times in minutes, hours and days', () => {
        expect(formatRelativeTime(at(35 * 60_000), NOW)).toMatch(/35 min/);
        expect(formatRelativeTime(at(2 * 3_600_000), NOW)).toMatch(/2 hr/);
        expect(formatRelativeTime(at(3 * 86_400_000), NOW)).toMatch(/3 days?/);
    });

    it('reads "now" for past times and for the next minute', () => {
        expect(formatRelativeTime(at(-10 * 60_000), NOW)).toBe('now');
        expect(formatRelativeTime(at(0), NOW)).toBe('now');
        expect(formatRelativeTime(at(59_999), NOW)).toBe('now');
    });

    it('switches units at the boundaries', () => {
        expect(formatRelativeTime(at(60_000), NOW)).toMatch(/1 min/);
        expect(formatRelativeTime(at(59 * 60_000), NOW)).toMatch(/59 min/);
        expect(formatRelativeTime(at(3_600_000), NOW)).toMatch(/1 hr/);
        expect(formatRelativeTime(at(86_400_000), NOW)).not.toMatch(/hr|min/);
    });

    it('returns invalid input unchanged', () => {
        expect(formatRelativeTime('soon', NOW)).toBe('soon');
    });
});
