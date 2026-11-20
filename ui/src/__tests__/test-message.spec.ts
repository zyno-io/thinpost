import { describe, expect, it } from 'vitest';

import {
    DEFAULT_TEST_SUBJECT,
    DEFAULT_TEST_TEXT,
    describeTestRefusal,
    formatFrom,
    isUnknownOutcome,
    MAX_TEST_RECIPIENTS,
    recipientProblem,
    validateDisplayName,
    validateLocalPart,
    validateSubject
} from '../test-message';
import { InvalidResponseError } from '../api-errors';
import { httpError, networkError } from './helpers';

describe('formatFrom', () => {
    it('returns the bare address without a display name', () => {
        expect(formatFrom('', 'test', 'example.com')).toBe('test@example.com');
        expect(formatFrom('   ', 'test', 'example.com')).toBe('test@example.com');
    });

    it('leaves a plain name unquoted', () => {
        expect(formatFrom('Example Team', 'test', 'example.com')).toBe('Example Team <test@example.com>');
    });

    it.each([
        ['Team, Example', '"Team, Example" <test@example.com>'],
        ['Say "hi"', '"Say \\"hi\\"" <test@example.com>'],
        ['back\\slash', '"back\\\\slash" <test@example.com>'],
        ['Ops (test)', '"Ops (test)" <test@example.com>'],
        ['Zoë', '"Zoë" <test@example.com>']
    ])('quotes %j', (name, expected) => {
        expect(formatFrom(name, 'test', 'example.com')).toBe(expected);
    });
});

describe('validators', () => {
    it('checks the local part', () => {
        expect(validateLocalPart('test')).toBeNull();
        expect(validateLocalPart('a.b+c')).toBeNull();
        expect(validateLocalPart('')).not.toBeNull();
        expect(validateLocalPart('a b')).not.toBeNull();
        expect(validateLocalPart('a@b')).not.toBeNull();
        expect(validateLocalPart('.a')).not.toBeNull();
        expect(validateLocalPart('a'.repeat(65))).not.toBeNull();
    });

    it('checks the display name', () => {
        expect(validateDisplayName('')).toBeNull();
        expect(validateDisplayName('Team')).toBeNull();
        expect(validateDisplayName('a\nb')).not.toBeNull();
        expect(validateDisplayName('x'.repeat(129))).not.toBeNull();
    });

    it('checks the subject in bytes', () => {
        expect(validateSubject(DEFAULT_TEST_SUBJECT)).toBeNull();
        expect(validateSubject('a\tb')).toBeNull();
        expect(validateSubject('  ')).not.toBeNull();
        expect(validateSubject('a\nb')).not.toBeNull();
        expect(validateSubject('é'.repeat(499))).toBeNull();
        expect(validateSubject('é'.repeat(500))).not.toBeNull();
    });

    it('limits recipients', () => {
        expect(recipientProblem(MAX_TEST_RECIPIENTS, 0)).toBeNull();
        expect(recipientProblem(MAX_TEST_RECIPIENTS + 1, 0)).not.toBeNull();
        expect(recipientProblem(1, 1)).toContain('invalid');
    });
});

describe('defaults', () => {
    it('are neutral', () => {
        const copy = `${DEFAULT_TEST_SUBJECT} ${DEFAULT_TEST_TEXT}`;
        expect(copy).not.toMatch(/@|https?:|\.(com|net|org)\b/i);
    });
});

describe('isUnknownOutcome', () => {
    it('is true without a definitive answer', () => {
        expect(isUnknownOutcome(networkError())).toBe(true);
        expect(isUnknownOutcome(httpError(503, { error: 'Service Unavailable' }))).toBe(true);
        expect(isUnknownOutcome(httpError(500))).toBe(true);
        expect(isUnknownOutcome(new InvalidResponseError())).toBe(true);
    });

    it('is false for refusals', () => {
        for (const status of [400, 401, 403, 404, 409, 422, 429]) expect(isUnknownOutcome(httpError(status))).toBe(false);
    });
});

describe('describeTestRefusal', () => {
    it('points a zero submission rate at the Sending limit panel', () => {
        const message = describeTestRefusal(httpError(429, { error: 'submission_rate_exceeded' })).message;
        expect(message).toContain('Sending limit');
        expect(message).toContain('API keys tab');
        expect(message).toContain('Nothing was sent');
    });

    it('explains the other limits', () => {
        expect(describeTestRefusal(httpError(429, { error: 'recipient_quota_exceeded' })).message).toContain('daily recipient limit');
        expect(describeTestRefusal(httpError(429, { error: 'pending_limit_exceeded' })).message).toContain('pending-message limit');
    });

    it('lists causes for a rejection and handles a paused app', () => {
        expect(describeTestRefusal(httpError(422, { error: 'message_rejected' })).message).toContain('verified sending domain');
        expect(describeTestRefusal(httpError(409, { error: 'app_paused' })).message).toContain('paused');
    });

    it('mentions an older server on 404 and falls back for other statuses', () => {
        expect(describeTestRefusal(httpError(404, { error: 'Not Found' })).message).toContain('upgrade');
        expect(describeTestRefusal(httpError(400, { error: 'Bad Request' })).kind).toBe('invalid');
    });
});
