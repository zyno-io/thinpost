import { describe, expect, it } from 'vitest';

import { safeFilename } from '@/attachment-download';

describe('safeFilename', () => {
    it('replaces path separators, control and reserved characters', () => {
        expect(safeFilename('a/b\\c:d*e?f"g<h>i|j\u0001k', 0)).toBe('a_b_c_d_e_f_g_h_i_j_k');
    });
    it('strips leading dots and trims', () => {
        expect(safeFilename('  ..hidden.txt ', 0)).toBe('hidden.txt');
    });
    it('neutralizes bidi overrides', () => {
        expect(safeFilename('invoice‮gpj.exe', 0)).toBe('invoice_gpj.exe');
    });
    it('cuts to 180 characters and falls back for empty names', () => {
        expect(safeFilename('a'.repeat(300), 0)).toHaveLength(180);
        expect(safeFilename('', 2)).toBe('attachment-3');
        expect(safeFilename('...', 0)).toBe('attachment-1');
    });
});
