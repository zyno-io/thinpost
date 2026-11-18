/**
 * TEST-ONLY fixtures for the dev-only Playwright checks. Nothing here ships in the console bundle.
 *
 * Every response below is a fabricated stand-in for the operator API (`/api/v1`) and for an OIDC
 * identity provider at https://idp.test, installed with `page.route`. It exists to drive the real console
 * UI in a real browser; it is not operational data and proves nothing about the real backend. Final
 * acceptance uses the real application and sign-in flow on staging.
 */
import { expect, type Page, type Route } from '@playwright/test';

export const APP_ID = '0123456789abcdef0123456789abcdef';
export const OTHER_APP_ID = '11111111111111111111111111111111';
export const MSG_PLAIN = 'a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1';
export const MSG_EP = 'b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2';
export const MSG_OLD = 'c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3';
/** A message whose subject, sender and recipients were not recorded. */
export const MSG_BARE = 'd4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4';
export const DELIVERY_A = 'd4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4';
export const DELIVERY_B = 'e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5';

// A fabricated signing secret returned by the mocked create and rotate endpoints.
export const WEBHOOK_SECRET = 'whsec_e2e_fabricated_secret';
export const IDP = 'https://idp.test';
export const CLIENT_ID = 'thinpost-e2e';
export const REMOTE_HOSTS = ['tracker.test', 'beacon.test'];

/** The production console CSP shape from internal/controlapp/console_test.go, with the test IdP origin. */
export const CONSOLE_CSP =
    "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
    `font-src 'self'; connect-src 'self' ${IDP}; frame-src 'none'; child-src 'none'; worker-src 'none'; ` +
    "object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'; manifest-src 'self'";

const PNG_1X1 = Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==',
    'base64'
);

/** URLs of requests that reached the network for a remote test host (CSP-blocked requests never do). */
export const remoteHits: string[] = [];

export const PREVIEW_VISIBLE_TEXT = 'PREVIEW VISIBLE CONTENT';

/** Hostile email HTML used by the isolation specs. */
export const HOSTILE_HTML = `<!doctype html><html><head>
<meta http-equiv="refresh" content="0;url=https://beacon.test/refresh">
<style>@import url(https://tracker.test/import.css); body { background: url(https://tracker.test/bg.png); } .x { background-image: url(/t/o/css) }</style>
<link rel="stylesheet" href="https://tracker.test/link.css">
</head><body background="https://tracker.test/body-bg.png">
<p>${PREVIEW_VISIBLE_TEXT}</p>
<img src="https://tracker.test/pixel.gif" width="1" height="1">
<img src="/t/o/relative-pixel.gif" width="1" height="1">
<img srcset="https://tracker.test/set-1x.png 1x, https://tracker.test/set-2x.png 2x" src="https://tracker.test/set.png">
<img src="x" onerror="fetch('https://beacon.test/onerror')">
<script>fetch('https://beacon.test/script')</script>
<iframe src="https://tracker.test/frame"></iframe>
<cid-holder><img src="cid:logo@example.com" alt="logo"></cid-holder>
<p><a href="https://beacon.test/click" target="_blank">CLICK THIS LINK</a></p>
</body></html>`;

interface MessageSeed {
    id: string;
    state: string;
    status: string;
    enhanced_protection: boolean;
    subject?: string;
    subject_masked: boolean;
    sender_domain?: string;
    recipients?: string[];
    recipient_count?: number;
    accepted_at: string;
    delivery_count: number;
    accepted_count: number;
    failed_count: number;
    uncertain_count: number;
}

const messages: MessageSeed[] = [
    {
        id: MSG_PLAIN,
        state: 'prepared',
        status: 'delivered',
        enhanced_protection: false,
        subject: 'Invoice 2026-11',
        subject_masked: false,
        sender_domain: 'billing.example.com',
        recipients: ['ada@customer.example.org', 'grace@customer.example.net'],
        recipient_count: 2,
        accepted_at: '2026-11-02T12:00:00Z',
        delivery_count: 2,
        accepted_count: 2,
        failed_count: 0,
        uncertain_count: 0
    },
    {
        id: MSG_EP,
        state: 'prepared',
        status: 'failed',
        enhanced_protection: true,
        subject_masked: true,
        sender_domain: 'clinic.example.com',
        recipients: ['patient@example.org'],
        recipient_count: 1,
        accepted_at: '2026-11-02T11:00:00Z',
        delivery_count: 1,
        accepted_count: 0,
        failed_count: 1,
        uncertain_count: 0
    },
    {
        id: MSG_OLD,
        state: 'prepared',
        status: 'retrying',
        enhanced_protection: false,
        subject: 'Password reset',
        subject_masked: false,
        sender_domain: 'accounts.example.com',
        recipients: ['sam@customer.example.org'],
        recipient_count: 1,
        accepted_at: '2026-10-01T09:00:00Z',
        delivery_count: 1,
        accepted_count: 0,
        failed_count: 0,
        uncertain_count: 0
    },
    {
        id: MSG_BARE,
        state: 'prepared',
        status: 'delivered',
        enhanced_protection: false,
        subject_masked: false,
        accepted_at: '2026-09-15T08:00:00Z',
        delivery_count: 1,
        accepted_count: 1,
        failed_count: 0,
        uncertain_count: 0
    }
];

function search(q: string, field: string): MessageSeed[] {
    const needle = q.toLowerCase();
    return messages.filter(m => {
        if (field === 'sender_domain') return !!m.sender_domain && (m.sender_domain === needle || m.sender_domain.endsWith(`.${needle}`));
        if (field === 'recipient_domain') return (m.recipients ?? []).some(r => r.split('@')[1] === needle);
        if (field === 'id') return m.id.startsWith(needle);
        if (field === 'subject') return !m.enhanced_protection && (m.subject ?? '').toLowerCase().includes(needle);
        return (m.subject ?? '').toLowerCase().includes(needle) || m.id.startsWith(needle) || (m.recipients ?? []).some(r => r.toLowerCase().startsWith(needle));
    });
}

export interface MockOptions {
    /** Stored HTML for the plain message body. */
    html?: string;
    /** Base64 header values for the IdP token. */
    signedIn?: boolean;
}

const json = (route: Route, body: unknown, status = 200) =>
    route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) });

function b64url(value: object): string {
    return Buffer.from(JSON.stringify(value)).toString('base64url');
}

/** Installs the fabricated operator API and the fabricated identity provider. */
export async function installMocks(page: Page, options: MockOptions = {}) {
    remoteHits.length = 0;
    let nonce = '';
    const cors = { 'access-control-allow-origin': '*', 'access-control-allow-headers': '*' };

    await page.route(`${IDP}/**`, async route => {
        const url = new URL(route.request().url());
        if (route.request().method() === 'OPTIONS') return route.fulfill({ status: 204, headers: cors });
        if (url.pathname === '/.well-known/openid-configuration') {
            return route.fulfill({
                status: 200,
                headers: cors,
                contentType: 'application/json',
                body: JSON.stringify({
                    issuer: IDP,
                    authorization_endpoint: `${IDP}/authorize`,
                    token_endpoint: `${IDP}/token`,
                    end_session_endpoint: `${IDP}/logout`,
                    jwks_uri: `${IDP}/jwks`
                })
            });
        }
        if (url.pathname === '/authorize') {
            nonce = url.searchParams.get('nonce') ?? '';
            const redirect = url.searchParams.get('redirect_uri')!;
            const state = url.searchParams.get('state')!;
            // A script redirect rather than a 302: WebKit cannot fulfill a route with a redirect status.
            const target = JSON.stringify(`${redirect}?code=test-code&state=${encodeURIComponent(state)}`).replace(/</g, '\\u003c');
            return route.fulfill({ status: 200, contentType: 'text/html', body: `<!doctype html><title>Test IdP</title><script>location.replace(${target})</script>` });
        }
        if (url.pathname === '/token') {
            const now = Math.floor(Date.now() / 1000);
            // Unsigned token: oidc-client-ts decodes the id_token without verifying its signature.
            const idToken = `${b64url({ alg: 'none', typ: 'JWT' })}.${b64url({
                iss: IDP,
                aud: CLIENT_ID,
                sub: 'e2e-operator',
                name: 'E2E Operator',
                iat: now,
                exp: now + 3600,
                ...(nonce ? { nonce } : {})
            })}.`;
            return route.fulfill({
                status: 200,
                headers: cors,
                contentType: 'application/json',
                body: JSON.stringify({ access_token: 'e2e-access-token', token_type: 'Bearer', expires_in: 3600, id_token: idToken, scope: 'openid' })
            });
        }
        if (url.pathname === '/logout') return route.fulfill({ status: 200, contentType: 'text/html', body: '<p>signed out</p>' });
        return route.fulfill({ status: 404, headers: cors });
    });

    await page.route('**/api/v1/**', async route => {
        const request = route.request();
        const url = new URL(request.url());
        const path = url.pathname.replace(/^\/api\/v1/, '');
        const method = request.method();

        if (path === '/config') return json(route, { oidc: { issuer: IDP, client_id: CLIENT_ID, scopes: ['openid', 'profile'] } });
        if (path === '/apps' && method === 'GET') {
            return json(route, {
                apps: [
                    { id: APP_ID, name: 'Billing', active: true, created_at: '2026-10-01T12:00:00Z', sending_ip_id: null },
                    { id: OTHER_APP_ID, name: 'Notifications', active: false, created_at: '2026-10-02T12:00:00Z', sending_ip_id: null }
                ]
            });
        }
        if (path === '/runtime') {
            return json(route, {
                features: { submission: true, tracking: true },
                tracking: { origin: 'https://t.example.test', link_event_limit: 25 }
            });
        }
        if (path === '/operations') {
            const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
            return json(route, {
                generated_at: new Date().toISOString(),
                gate: { mode: 'open', revision: 12, changed_at: ago(600), changed_by: 'deploy:thinpost/control:r41', generation_matches: true, quarantine_complete: true },
                workers: [{ worker_id: 'edge-staging-1', controller_id: 'control-7d9f', epoch: 6, expires_at: new Date(Date.now() + 75_000).toISOString(), live: true }],
                messages: { queued_due: 2, queued_waiting: 0, preparing: 1, preparation_failed: 1, recovery_hold: 0, oldest_due_at: ago(4) },
                deliveries: { queued_due: 3, queued_waiting: 12, in_flight: 2, uncertain: 1, recipient_policy_held: 1, tracking_held: 0, recovery_hold: 0, oldest_due_at: ago(37) },
                webhooks: { queued_due: 0, queued_waiting: 1, in_flight: 0, recovery_hold: 0 },
                retry_policy: { max_attempts: 128, initial_delay_ms: 1_800_000, max_delay_ms: 10_800_000, max_age_ms: 432_000_000, revision: 3, changed_at: ago(2000), changed_by: 'migration:033' }
            });
        }
        if (path === '/operations/attention') {
            return json(route, {
                items: [
                    { app_id: APP_ID, app_name: 'Billing', message_id: MSG_PLAIN, delivery_id: DELIVERY_A, kind: 'uncertain', since: '2026-10-05T09:30:00Z' },
                    { app_id: APP_ID, app_name: 'Billing', message_id: MSG_OLD, kind: 'preparation_failed', since: '2026-10-06T11:00:00Z' },
                    { app_id: OTHER_APP_ID, app_name: 'Notifications', message_id: MSG_EP, delivery_id: DELIVERY_B, kind: 'recipient_policy_held', since: '2026-10-07T08:15:00Z' }
                ]
            });
        }
        if (path === '/sending-ips' && method === 'GET') {
            return json(route, { ips: [], unpinned_app_count: 2, edge_reported_at: null, edge_reports_sources: false, edge_primary_address: null, unnamed_edge_addresses: [] });
        }
        if (path === '/retry-policy' && method === 'PUT') {
            return json(route, { ...(request.postDataJSON() as object), revision: 4, changed_at: new Date().toISOString(), changed_by: 'operator' });
        }
        const m = path.match(/^\/apps\/([0-9a-f]{32})(\/.*)?$/);
        if (!m) return json(route, { error: 'not found' }, 404);
        const rest = m[2] ?? '';

        if (rest === '/keys') return json(route, { keys: [{ id: 'f6'.repeat(16), name: 'Production sender', active: true, created_at: '2026-10-03T12:00:00Z' }] });
        if (rest === '/submission-rate') return json(route, { per_minute: 6, burst: 4 });
        if (rest === '/sending-ip') return json(route, { sending_ip_id: null, name: null });
        if (rest === '/tracking-defaults') return json(route, method === 'PUT' ? (request.postDataJSON() as object) : { opens: false, clicks: true });
        if (rest === '/tracking-quota') return json(route, { daily_limit: 500, used: 120, suppressed: 7, day: '2026-11-11' });
        if (rest === '/domains') {
            return json(route, { domains: [{ name: 'billing.example.com', active: true, created_at: '2026-10-04T12:00:00Z', verified_at: '2026-10-04T13:00:00Z' }] });
        }
        const hookId = '0a'.repeat(16);
        const hook = {
            id: hookId,
            format: 'thinpost',
            endpoint: 'https://hooks.example.com/thinpost',
            description: 'Billing receiver',
            active: true,
            created_at: '2026-10-05T12:00:00Z',
            secret_created_at: '2026-10-05T12:00:00Z'
        };
        // A Postmark-compatible endpoint with Basic auth and one header: no signing secret, and values are never returned.
        const providerHook = {
            id: '0c'.repeat(16),
            format: 'postmark',
            endpoint: 'https://hooks.example.com/postmark',
            description: 'Postmark-style receiver',
            events: ['Delivery', 'Bounce'],
            active: true,
            created_at: '2026-10-06T12:00:00Z',
            auth: { username: 'thinpost', header_names: ['X-Api-Key'] }
        };
        if (rest === '/webhooks' && method === 'GET') return json(route, { webhooks: [hook, providerHook] });
        if (rest === '/webhooks' && method === 'POST') {
            type Created = { format?: string; endpoint: string; description?: string; events?: string[]; auth?: { username?: string; headers?: Array<{ name: string }> } };
            const { auth, ...body } = request.postDataJSON() as Created;
            const created: Record<string, unknown> = {
                ...hook,
                id: '0b'.repeat(16),
                description: '',
                ...body,
                ...(auth ? { auth: { ...(auth.username ? { username: auth.username } : {}), header_names: (auth.headers ?? []).map(h => h.name) } } : {})
            };
            if (body.format === 'postmark') {
                delete created.secret_created_at;
                return json(route, { webhook: created }, 201);
            }
            return json(route, { webhook: created, secret: WEBHOOK_SECRET }, 201);
        }
        if (rest === `/webhooks/${hookId}` && method === 'PATCH') return json(route, { ...hook, ...(request.postDataJSON() as object) });
        if (rest === `/webhooks/${hookId}` && method === 'DELETE') return route.fulfill({ status: 204, headers: { 'cache-control': 'no-store' } });
        if (rest === `/webhooks/${hookId}/rotate-secret` && method === 'POST') return json(route, { webhook: hook, secret: WEBHOOK_SECRET });
        if (rest === `/webhooks/${hookId}/test` && method === 'POST') return json(route, { success: true, http_status: 204 });
        if (rest === '/webhook-deliveries') return json(route, { deliveries: [] });

        if (rest.startsWith('/suppressions')) {
            // Fabricated list; the console never ships this data.
            const entries = [
                { id: '91'.repeat(16), address: 'bounced.user@example.org', reason: 'hard_bounce', origin: 'recipient', created_at: '2026-10-05T09:30:00Z', source_message_id: MSG_PLAIN },
                { id: '92'.repeat(16), address: 'unsubscribed@example.net', reason: 'unsubscribe', origin: 'recipient', created_at: '2026-10-04T09:30:00Z' },
                { id: '93'.repeat(16), address: 'blocked.by.operator@a-very-long-domain-name.example.com', reason: 'manual', origin: 'console', created_at: '2026-10-03T09:30:00Z' }
            ];
            if (rest === '/suppressions' && method === 'GET') return json(route, { suppressions: entries });
            if (rest === '/suppressions/search' && method === 'POST') {
                const q = (request.postDataJSON() as { q: string }).q.toLowerCase();
                return json(route, { suppressions: entries.filter(e => e.address.toLowerCase().startsWith(q)) });
            }
            if (rest === '/suppressions' && method === 'POST') {
                const body = request.postDataJSON() as { addresses: string[] };
                return json(route, { results: body.addresses.map(address => ({ address, status: 'added', suppression: { ...entries[2], address } })) });
            }
            if (rest === '/suppressions/delete' && method === 'POST') return json(route, { deleted: (request.postDataJSON() as { ids: string[] }).ids.length });
        }
        if (rest === '/messages' && method === 'GET') return json(route, { messages: messages.map(listView) });
        if (rest === '/messages/search' && method === 'POST') {
            const body = request.postDataJSON() as { q: string; field?: string };
            return json(route, { messages: search(body.q, body.field ?? 'any').map(listView) });
        }
        const detail = rest.match(/^\/messages\/([0-9a-f]{32})(\/.*)?$/);
        if (detail) {
            const seed = messages.find(x => x.id === detail[1]);
            if (!seed) return json(route, { error: 'not found' }, 404);
            const sub = detail[2] ?? '';
            if (sub === '') {
                return json(route, {
                    message: detailView(seed),
                    deliveries: [
                        seed.status === 'retrying'
                            ? {
                                  id: DELIVERY_A,
                                  recipient_index: 0,
                                  recipient: seed.recipients?.[0],
                                  state: 'queued',
                                  retry_attempts: 2,
                                  next_attempt_at: new Date(Date.now() + 35 * 60_000).toISOString(),
                                  last_completed_outcome: 'rejected',
                                  last_smtp_code: 451
                              }
                            : seed.failed_count
                              ? { id: DELIVERY_A, recipient_index: 0, recipient: seed.recipients?.[0], state: 'failed', retry_attempts: 1, last_completed_outcome: 'rejected', last_smtp_code: 550 }
                              : {
                                    id: DELIVERY_A,
                                    recipient_index: 0,
                                    recipient: seed.recipients?.[0],
                                    state: 'accepted',
                                    retry_attempts: 1,
                                    last_completed_outcome: 'accepted',
                                    last_smtp_code: 250,
                                    ...(seed.id === MSG_PLAIN ? { tracking: { opens: 3, clicks: 1, last_event_at: '2026-10-05T10:05:00Z' } } : {})
                                },
                        ...(seed.delivery_count > 1
                            ? [{ id: DELIVERY_B, recipient_index: 1, recipient: seed.recipients?.[1], state: 'accepted', retry_attempts: 1, last_completed_outcome: 'accepted', last_smtp_code: 250 }]
                            : [])
                    ].map(d => (d.recipient ? d : { ...d, recipient: undefined }))
                });
            }
            if (sub === '/subject') {
                return seed.enhanced_protection ? json(route, { subject: 'Confidential: test results' }) : json(route, { error: 'conflict' }, 409);
            }
            if (sub === '/body') {
                return json(route, {
                    from: `sender@${seed.sender_domain ?? 'example.com'}`,
                    to: seed.recipients ?? [],
                    subject: seed.subject ?? 'Confidential: test results',
                    text: 'Plain text part of the message.',
                    html: options.html ?? HOSTILE_HTML,
                    enhanced_protection: seed.enhanced_protection,
                    attachments: [
                        { index: 0, filename: 'logo.png', content_type: 'image/png', size: PNG_1X1.length, inline: true, content_id: 'logo@example.com' },
                        { index: 1, filename: '../../report: Q4?.pdf', content_type: 'application/pdf', size: 2048, inline: false }
                    ]
                });
            }
            const attachment = sub.match(/^\/attachments\/(\d+)$/);
            if (attachment) {
                return route.fulfill({
                    status: 200,
                    contentType: 'application/octet-stream',
                    headers: { 'cache-control': 'no-store' },
                    body: attachment[1] === '0' ? PNG_1X1 : Buffer.from('%PDF-1.4 fabricated test attachment')
                });
            }
            if (sub.match(/^\/deliveries\/[0-9a-f]{32}\/headers$/)) {
                return json(route, {
                    headers: [
                        { name: 'Received', value: 'from mail.example.com by mx.test' },
                        { name: 'Subject', value: '=?UTF-8?B?SW52b2ljZQ==?=', decoded_value: 'Invoice' },
                        { name: 'To', value: seed.recipients?.[0] ?? 'unknown' }
                    ]
                });
            }
        }
        return json(route, { error: 'not found' }, 404);
    });

    // Answer the remote hosts the preview must never reach, and count every request that actually leaves the
    // renderer for them. A request blocked by CSP never reaches this handler, which is what the specs assert.
    for (const host of REMOTE_HOSTS) {
        await page.route(`**://${host}/**`, route => {
            remoteHits.push(route.request().url());
            return route.fulfill({ status: 204 });
        });
    }
}

function listView(seed: MessageSeed) {
    const { subject, recipients, ...rest } = seed;
    return {
        ...rest,
        ...(subject !== undefined && !seed.enhanced_protection ? { subject } : {}),
        ...(recipients ? { recipients: recipients.slice(0, 3) } : {})
    };
}

function detailView(seed: MessageSeed) {
    const { subject, recipients, ...rest } = seed;
    return {
        ...rest,
        ...(subject !== undefined && !seed.enhanced_protection ? { subject } : {}),
        ...(recipients ? { recipients } : {})
    };
}

/**
 * Adds the production console CSP to document responses so the preview is exercised under the real policy.
 * Used by the `csp` projects, which serve the production build with `vite preview`.
 */
export async function injectConsoleCsp(page: Page) {
    await page.route('**/*', async route => {
        const request = route.request();
        if (request.resourceType() !== 'document' || !request.url().startsWith('http://localhost')) return route.fallback();
        const response = await route.fetch();
        await route.fulfill({ response, headers: { ...response.headers(), 'content-security-policy': CONSOLE_CSP } });
    });
}

/**
 * Opens `path` signed in. Tokens live only in memory, so a reload signs out; instead the sign-in screen is
 * shown at `path` and the mocked identity provider returns to it.
 */
export async function openSignedIn(page: Page, path: string) {
    await page.goto(path);
    // A session persisted by an earlier sign-in in this page's storage is restored without the IdP round trip.
    const signIn = page.getByRole('button', { name: /sign in/i }).first();
    const main = page.locator('main#main-content');
    await signIn.or(main).first().waitFor();
    if (await signIn.isVisible()) {
        await signIn.click();
        await page.waitForURL(url => url.pathname === path.split('?')[0], { timeout: 15_000 });
    }
    await expect(page.locator('main#main-content')).toBeVisible();
}

/** Records every request the page makes, with its method, URL and body. */
export function recordRequests(page: Page) {
    const seen: Array<{ method: string; url: string; body: string | null }> = [];
    page.on('request', request => seen.push({ method: request.method(), url: request.url(), body: request.postData() }));
    return seen;
}
