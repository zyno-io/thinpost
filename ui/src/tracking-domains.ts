import { LOOKUP_NOTE, MATCHES_NOTE, type BounceNameInput as NameInput, type CnameRecordView, type StatusTag } from './bounce-domains';
import { normalizeDomain } from './validation';

/** The longest tracking domain Thinpost accepts. */
export const MAX_TRACKING_DOMAIN_LENGTH = 253;

export type TrackingState = 'pending' | 'active' | 'failing' | 'removed';
export type TrackingDnsResult = 'ok' | 'missing' | 'wrong_target' | 'lookup_failed';
export type TrackingHttpsResult = 'ok' | 'unreachable' | 'certificate' | 'redirected' | 'not_thinpost' | 'not_found';

/** A tracking domain as the API sends it, read tolerantly. */
export interface TrackingView {
    name: string;
    state: TrackingState;
    checking: boolean;
    dnsResult: TrackingDnsResult | null;
    httpsResult: TrackingHttpsResult | null;
    observedTarget: string | null;
    certificateExpiresAt: string | null;
    checkedAt: string | null;
    verifiedAt: string | null;
}

const STATES: readonly string[] = ['pending', 'active', 'failing', 'removed'];
const DNS_RESULTS: readonly string[] = ['ok', 'missing', 'wrong_target', 'lookup_failed'];
const HTTPS_RESULTS: readonly string[] = ['ok', 'unreachable', 'certificate', 'redirected', 'not_thinpost', 'not_found'];

const text = (value: unknown): string | null => (typeof value === 'string' && value ? value : null);

/** Reads a tracking domain; null when it is absent or malformed, so a bad body never renders as a state. */
export function parseTrackingDomain(value: unknown): TrackingView | null {
    if (typeof value !== 'object' || value === null) return null;
    const row = value as Record<string, unknown>;
    const name = text(row.name);
    if (!name || typeof row.state !== 'string' || !STATES.includes(row.state)) return null;
    return {
        name,
        state: row.state as TrackingState,
        checking: row.checking === true,
        dnsResult: typeof row.dns_result === 'string' && DNS_RESULTS.includes(row.dns_result) ? (row.dns_result as TrackingDnsResult) : null,
        httpsResult: typeof row.https_result === 'string' && HTTPS_RESULTS.includes(row.https_result) ? (row.https_result as TrackingHttpsResult) : null,
        observedTarget: text(row.observed_target),
        certificateExpiresAt: text(row.certificate_expires_at),
        checkedAt: text(row.checked_at),
        verifiedAt: text(row.verified_at)
    };
}

/** What the deployment offers: whether apps may add their own tracking domain, and the name to point DNS at. */
export interface TrackingSupport {
    available: boolean;
    /** The deployment's tracking host, for the DNS record; null when not reported. */
    cnameTarget: string | null;
}

export type TrackingStatus = StatusTag;

/** The HTTPS row: whether Thinpost can open the name securely, and the certificate's expiry. */
export interface HttpsView {
    tag: StatusTag;
    /** One short line, only for a problem someone must act on. */
    line: string | null;
    expiresAt: string | null;
}

export interface TrackingPanelView {
    tag: StatusTag;
    record: CnameRecordView;
    https: HttpsView | null;
    /** One short line, only when it says something to act on. */
    note: string | null;
}

const HTTPS_PROBLEMS: Record<Exclude<TrackingHttpsResult, 'ok'>, { label: string; line: string | null }> = {
    certificate: { label: 'No valid certificate', line: 'Your Thinpost administrator issues it.' },
    unreachable: { label: "Can't connect", line: 'Your Thinpost administrator finishes setup.' },
    redirected: { label: 'Redirects', line: 'Links must pass through, not redirect.' },
    not_thinpost: { label: 'Not this installation', line: 'The name answers from somewhere else.' },
    not_found: { label: 'Not in DNS', line: null }
};

/** What is wrong with the DNS record, if anything. A name behind a proxy never matches the record and still works, so only a name that cannot be reached counts. */
function dnsFault(view: TrackingView): 'add' | 'update' | null {
    if (!view.httpsResult) return 'add';
    if (view.dnsResult === 'missing') return 'add';
    if (view.dnsResult === 'wrong_target' && view.httpsResult === 'unreachable') return 'update';
    return null;
}

/**
 * The short tag for a tracking domain. The DNS result is advice only: what decides is the secure connection.
 */
export function trackingStatus(view: TrackingView, support: TrackingSupport): TrackingStatus {
    if (!support.available) return { label: 'Not in use', tone: 'warning' };
    if (view.state === 'active') return { label: 'In use', tone: 'success' };
    if (view.state === 'removed') return { label: 'Removed', tone: 'warning' };
    if (view.state === 'failing') return { label: 'Not in use', tone: 'warning' };
    return { label: dnsFault(view) ? 'Waiting for DNS record' : 'Waiting for HTTPS', tone: 'warning' };
}

function httpsView(view: TrackingView): HttpsView | null {
    if (view.httpsResult === 'ok') return { tag: { label: 'Secure', tone: 'success' }, line: null, expiresAt: view.certificateExpiresAt };
    if (!view.httpsResult) return null;
    const problem = HTTPS_PROBLEMS[view.httpsResult];
    const settled = view.state === 'active';
    return {
        tag: { label: settled ? 'Last check failed' : problem.label, tone: 'warning' },
        line: settled ? "Last check didn't finish. Thinpost will retry." : problem.line,
        expiresAt: view.certificateExpiresAt
    };
}

/** What the Record and HTTPS rows of a tracking domain show. Why it helps is in the help text. */
export function trackingView(view: TrackingView, support: TrackingSupport): TrackingPanelView {
    const tag = trackingStatus(view, support);
    const point = support.cnameTarget;
    const record: CnameRecordView = { name: view.name, target: point, observed: null, fix: null };
    const https = httpsView(view);
    if (!support.available) {
        // Switched off for the installation, but the certificate's expiry is still worth seeing.
        return { tag, record, https: view.certificateExpiresAt ? https : null, note: 'Not turned on for this installation. Ask your Thinpost administrator.' };
    }
    if (view.state === 'removed') return { tag, record, https: null, note: 'No longer used for new mail.' };

    const settled = view.state === 'active';
    const seen = view.observedTarget;
    const dangling = view.dnsResult === 'missing' && !!seen;
    const elsewhere = view.dnsResult === 'wrong_target' && (!!seen || (!settled && view.httpsResult === 'unreachable'));
    let note: string | null = null;
    if (point && seen === point && view.dnsResult === 'wrong_target') {
        // The last result is old news: the record has since been set to what is wanted.
        note = MATCHES_NOTE;
    } else if (dangling || elsewhere) {
        const proxied = elsewhere && (settled || view.httpsResult === 'ok');
        record.observed = {
            state: 'problem',
            text: seen ?? 'another name',
            hint: dangling ? "Points at a name that doesn't resolve" : proxied ? 'Fine only behind a proxy' : 'Points at the wrong name'
        };
        if (point && !settled && !proxied && seen !== point) {
            record.fix = { kind: 'update', host: view.name, value: point };
            if (elsewhere) note = 'Behind a proxy such as Cloudflare? Then only HTTPS must work.';
        }
    } else if (!settled) {
        const lookupFailed = view.dnsResult === 'lookup_failed' && view.httpsResult !== 'ok';
        if (dnsFault(view) === 'add' || lookupFailed) {
            if (!point) {
                note = 'Name to point at not known yet. Ask your Thinpost administrator.';
            } else {
                if (!lookupFailed) record.observed = { state: 'missing', text: point, hint: view.dnsResult === 'missing' ? 'Not found in DNS' : 'Not added yet' };
                record.fix = { kind: 'add', host: view.name, value: point };
            }
        }
        if (!note && view.dnsResult === 'lookup_failed') note = LOOKUP_NOTE;
    }
    // DNS comes first: no point telling the administrator to finish setup while the record is wrong.
    if (record.fix && https?.line && view.httpsResult === 'unreachable') https.line = null;
    return { tag, record, https, note };
}

/** Mirrors the API's rules: a strict subdomain of the sending domain, at most 253 characters. The server decides. */
export function validateTrackingName(value: string, sender: string): NameInput {
    const parsed = normalizeDomain(value);
    if (parsed.domain === null) return { name: null, error: parsed.error };
    if (parsed.domain.length > MAX_TRACKING_DOMAIN_LENGTH) return { name: null, error: `Use at most ${MAX_TRACKING_DOMAIN_LENGTH} characters.` };
    if (!parsed.domain.endsWith(`.${sender}`) || parsed.domain.length <= sender.length + 1) {
        return { name: null, error: `Use a subdomain of ${sender}, such as links.${sender}.` };
    }
    return { name: parsed.domain, error: null };
}

/** The name suggested for a sending domain. */
export const suggestTrackingName = (sender: string) => `links.${sender}`;

/** One short line for the Tracking tab's read-only summary. */
export function trackingSummaryLine(view: TrackingView): string {
    if (view.state === 'active') return 'In use';
    if (view.state === 'failing') return 'Not in use';
    return 'Waiting for setup';
}
