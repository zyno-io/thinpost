import { describe, expect, it } from 'vitest';

import { validateWebhookDescription, validateWebhookEndpoint } from '@/validation';

// The valid and invalid lists mirror internal/admin/webhook_endpoint_test.go.
describe('validateWebhookEndpoint', () => {
    it.each([
        'https://Example.COM/hook',
        'https://example.com',
        'https://example.com:/h',
        'https://example.com:443/a/b',
        'https://example.com/a%2Fb',
        'https://mx.example.net/x?a=1&b=',
        'https://example.com?token=Ab',
        'https://xn--bcher-kva.example/h',
        'https://example.com/?a=%41%2f',
        'https://1st.example/h',
        'https://a.b123/h',
        'https://example.local.example/h',
        'https://example.com/' + 'a'.repeat(2048 - 'https://example.com/'.length),
        // Path escapes grow only the path; the same characters in the query are not re-encoded.
        'https://example.com/?q=' + '{'.repeat(2048 - 'https://example.com/?q='.length),
        'https://example.com/' + '{'.repeat(676)
    ])('accepts %s', value => {
        expect(validateWebhookEndpoint(value)).toBeNull();
    });

    it.each([
        ['', 'Enter a webhook address.'],
        ['http://example.com/h', 'Use an https:// address.'],
        ['ftp://example.com/', 'Use an https:// address.'],
        ['//example.com/h', 'Use an https:// address.'],
        ['example.com/h', 'Use an https:// address.'],
        ['https:example.com', 'Use an https:// address.'],
        ['mailto:a@example.com', 'Use an https:// address.'],
        ['https:///h', 'Enter a host name.'],
        ['https://example.com:8443/h', 'Use port 443, or leave the port out.'],
        ['https://127.0.0.1/h', 'Use a host name, not an IP address.'],
        ['https://[::1]/h', 'Use a host name, not an IP address.'],
        ['https://[::1]:443/h', 'Use a host name, not an IP address.'],
        ['https://127.1/h', 'numeric shorthand'],
        ['https://10.1/h', 'numeric shorthand'],
        ['https://192.168.1/h', 'numeric shorthand'],
        ['https://0x7f.0.0.1/h', 'numeric shorthand'],
        ['https://a.0x1F/h', 'numeric shorthand'],
        ['https://a.0x/h', 'numeric shorthand'],
        ['https://localhost/h', 'Enter a full host name'],
        ['https://x.localhost/', 'Use a public host name'],
        ['https://kubernetes.default.svc.cluster.local/', 'Use a public host name'],
        ['https://thinpost.thinpost.svc/', 'Use a public host name'],
        ['https://metadata.google.internal/', 'Use a public host name'],
        ['https://router.home.arpa/', 'Use a public host name'],
        ['https://printer.local/', 'Use a public host name'],
        ['https://example/h', 'Enter a full host name'],
        ['https://user@example.com/h', 'Remove the user name or password.'],
        ['https://user:pw@example.com/h', 'Remove the user name or password.'],
        ['https://example.com/h?', 'Remove the trailing'],
        ['https://example.com/h#frag', 'Remove the # and everything after it.'],
        ['https://example.com/h#', 'Remove the # and everything after it.'],
        ['https://example.com/h?a=1#x', 'Remove the # and everything after it.'],
        ['https://example.com\\@evil.example/h', 'Remove backslashes'],
        ['https://example.com/a\\b', 'Remove backslashes'],
        ['https://example.com/a b', 'Remove spaces and control characters'],
        ['https://example.com/a\tb', 'Remove spaces and control characters'],
        ['https://example.com/a\nb', 'Remove spaces and control characters'],
        ['https://example.com/a\x00b', 'Remove spaces and control characters'],
        ['https://example.com/a\x7fb', 'Remove spaces and control characters'],
        ['https://example.com/?a=\x01', 'Remove spaces and control characters'],
        ['https://exa mple.example/', 'Remove spaces and control characters'],
        ['https://bücher.example/h', 'Use plain ASCII only'],
        ['https://example.com/?q=ü', 'Use plain ASCII only'],
        ['https://example.com/?q=\u0085', 'Use plain ASCII only'],
        ['https://example.com/?q=‮', 'Use plain ASCII only'],
        ['https://example.com/​', 'Use plain ASCII only'],
        ['https://-a.example.com/', "can't start or end with a hyphen"],
        ['https://example.com/%zz', 'must be followed by two hex digits'],
        ['https://example.com/?a=%zz', 'must be followed by two hex digits'],
        ['https://example.com/?a=%4', 'must be followed by two hex digits'],
        ['https://example.com/?a=%', 'must be followed by two hex digits'],
        ['https://example.com/' + 'a'.repeat(2048), 'Use at most 2048 bytes.'],
        ['https://example.com/?' + 'a'.repeat(2048), 'Use at most 2048 bytes.'],
        // 2000 raw bytes, but each of these path characters normalizes to 3 bytes (> 2048).
        ...['"', '<', '>', '{', '}', '|', '^', '`'].map(ch => ['https://example.com/' + ch.repeat(2000 - 'https://example.com/'.length), 'after normalization'] as [string, string])
    ])('rejects %j', (value, message) => {
        expect(validateWebhookEndpoint(value)).toContain(message);
    });

    it('allows a query string', () => {
        expect(validateWebhookEndpoint('https://example.com/h?token=abc')).toBeNull();
    });
});

describe('validateWebhookDescription', () => {
    it('allows empty text and at most 256 bytes, without surrounding space or controls', () => {
        expect(validateWebhookDescription('')).toBeNull();
        expect(validateWebhookDescription('Billing receiver')).toBeNull();
        expect(validateWebhookDescription('a'.repeat(256))).toBeNull();
        expect(validateWebhookDescription('a'.repeat(257))).toContain('at most 256 bytes');
        expect(validateWebhookDescription(' x')).toContain('whitespace');
        expect(validateWebhookDescription('a\nb')).toContain('control');
    });
});

describe('webhook authentication validation', () => {
    it('applies the server header rules', async () => {
        const { validateWebhookHeader } = await import('@/validation');
        expect(validateWebhookHeader('X-Api-Key', 'v', false)).toBeNull();
        expect(validateWebhookHeader('X_Api', 'v', false)).toMatch(/underscore/);
        expect(validateWebhookHeader('Bad Name', 'v', false)).toMatch(/header name/);
        expect(validateWebhookHeader('Host', 'v', false)).toMatch(/reserved/);
        expect(validateWebhookHeader('content-type', 'v', false)).toMatch(/reserved/);
        expect(validateWebhookHeader('Svix-Id', 'v', false)).toMatch(/reserved/);
        expect(validateWebhookHeader('x-thinpost-event-id', 'v', false)).toMatch(/reserved/);
        expect(validateWebhookHeader('Proxy-Authorization', 'v', false)).toMatch(/reserved/);
        expect(validateWebhookHeader('Authorization', 'Bearer t', false)).toBeNull();
        expect(validateWebhookHeader('Authorization', 'Bearer t', true)).toMatch(/reserved/);
        expect(validateWebhookHeader('X-A', '', false)).toMatch(/value/);
        expect(validateWebhookHeader('X-A', '', false, true)).toBeNull();
        expect(validateWebhookHeader('X-A', 'bad\nvalue', false)).toMatch(/ASCII/);
        expect(validateWebhookHeader('X'.repeat(65), 'v', false)).toMatch(/64/);
    });

    it('applies the server Basic rules', async () => {
        const { validateWebhookBasic } = await import('@/validation');
        expect(validateWebhookBasic('', '')).toEqual({ username: null, password: null });
        expect(validateWebhookBasic('u', 'p')).toEqual({ username: null, password: null });
        expect(validateWebhookBasic('u', '').password).toBe('Enter a password.');
        expect(validateWebhookBasic('', 'p').username).toBe('Enter a user name.');
        expect(validateWebhookBasic('a:b', 'p').username).toMatch(/colon/);
    });

    it('splits user information out of a pasted URL', async () => {
        const { splitEndpointUserinfo } = await import('@/validation');
        expect(splitEndpointUserinfo('https://u:p@hooks.example.com/x')).toEqual({ endpoint: 'https://hooks.example.com/x', username: 'u', password: 'p' });
        expect(splitEndpointUserinfo('https://hooks.example.com/x@y')).toBeNull();
        expect(splitEndpointUserinfo('https://hooks.example.com/')).toBeNull();
    });
});
