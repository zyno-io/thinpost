import { describe, expect, it } from 'vitest';

import { attemptOutcome, hasDeliveryProgress, readDeliveryProgress, smtpCodeLabel } from '@/delivery-progress';

const base = { id: 'a'.repeat(32), recipient_index: 0, state: 'queued' };

describe('delivery progress fields', () => {
    it('treats a delivery from an older server as reporting nothing', () => {
        expect(hasDeliveryProgress(base)).toBe(false);
        expect(hasDeliveryProgress({ ...base, last_smtp_code: null })).toBe(false);
        expect(hasDeliveryProgress(null)).toBe(false);
        expect(readDeliveryProgress(base)).toEqual({
            retryAttempts: null,
            nextAttemptAt: null,
            hasCompletedAttempt: false,
            lastOutcome: null,
            lastOutcomeKey: null,
            lastSmtpCode: null
        });
    });

    it('reads valid fields', () => {
        const progress = readDeliveryProgress({
            ...base,
            retry_attempts: 3,
            next_attempt_at: '2026-11-02T15:30:00Z',
            last_completed_outcome: 'route_rejected',
            last_smtp_code: 421
        });
        expect(hasDeliveryProgress({ ...base, retry_attempts: 0 })).toBe(true);
        expect(progress.retryAttempts).toBe(3);
        expect(progress.nextAttemptAt).toBe('2026-11-02T15:30:00Z');
        expect(progress.hasCompletedAttempt).toBe(true);
        expect(progress.lastOutcome?.label).toBe('Rejected by one mail server');
        expect(progress.lastSmtpCode).toBe(421);
    });

    it.each([
        ['retry_attempts', -1],
        ['retry_attempts', 1.5],
        ['retry_attempts', '2'],
        ['next_attempt_at', 'tomorrow'],
        ['next_attempt_at', 1730561400],
        ['next_attempt_at', `2026-11-02T15:30:00Z${' '.repeat(80)}`],
        ['last_smtp_code', 600],
        ['last_smtp_code', -1],
        ['last_smtp_code', '250']
    ])('rejects a malformed %s of %j', (field, value) => {
        const progress = readDeliveryProgress({ ...base, [field]: value });
        expect(progress.retryAttempts).toBeNull();
        expect(progress.nextAttemptAt).toBeNull();
        expect(progress.lastSmtpCode).toBeNull();
    });

    it('never echoes an unrecognized outcome', () => {
        const unrecognized = [
            '550 5.1.1 <ada@example.org> User unknown',
            'Mailbox full',
            'a'.repeat(65),
            '',
            42,
            {},
            'greylisted',
            'constructor',
            '__proto__',
            'Accepted'
        ];
        for (const raw of unrecognized) {
            expect(attemptOutcome(raw)).toEqual({
                label: 'Unrecognized outcome',
                tone: 'inactive',
                description: "This console doesn't recognize the reported outcome, so it isn't shown."
            });
        }
        expect(attemptOutcome('accepted').description).toContain("doesn't guarantee it reached the inbox");
        for (const local of [
            'null_mx',
            'domain_not_found',
            'recipient_policy_denied',
            'tracking_unavailable',
            'retry_expired',
            'suppressed'
        ]) {
            expect(attemptOutcome(local).description).toContain("not a reply from the recipient's mail server");
        }
    });

    it('does not echo an identifier-shaped outcome that carries recipient data', () => {
        const sensitive = 'rejected_user_unknown_ada_lovelace_example_org';
        const progress = readDeliveryProgress({ ...base, last_completed_outcome: sensitive, last_smtp_code: 550 });
        expect(progress.hasCompletedAttempt).toBe(true);
        expect(progress.lastOutcome?.label).toBe('Unrecognized outcome');
        expect(progress.lastSmtpCode).toBe(550);
        const shown = JSON.stringify(progress);
        for (const fragment of [sensitive, 'ada', 'lovelace', 'example_org']) {
            expect(shown).not.toContain(fragment);
        }
    });

    it('labels SMTP codes without presenting acceptance as mailbox delivery', () => {
        expect(smtpCodeLabel(0).text).toBe('No reply recorded');
        expect(smtpCodeLabel(250).text).toBe('SMTP 250 (accepted)');
        expect(smtpCodeLabel(250).description).toContain("doesn't guarantee it reached the inbox");
        expect(smtpCodeLabel(451).text).toBe('SMTP 451 (temporary)');
        expect(smtpCodeLabel(550).text).toBe('SMTP 550 (permanent)');
        expect(smtpCodeLabel(354).text).toBe('SMTP 354');
    });
});
