import type { BounceNameInput as NameInput, Tone } from './bounce-domains';
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

export interface TrackingStatus {
    label: string;
    tone: Tone;
    /** Plain-language explanation for app developers and support staff. */
    message: string;
}

/** Why Thinpost can't use the address yet, in one sentence, from the HTTPS check. */
function connectionProblem(view: TrackingView): string {
    const name = view.name;
    switch (view.httpsResult) {
        case 'certificate':
            return `${name} doesn't have a valid security certificate yet.`;
        case 'redirected':
            return `${name} sends visitors somewhere else instead of passing tracking links through.`;
        case 'not_thinpost':
            return `${name} answers, but not from this Thinpost installation.`;
        case 'not_found':
            return `Thinpost can't find ${name} in DNS.`;
        case 'unreachable':
            return `Thinpost couldn't reach ${name} over a secure connection.`;
        default:
            return '';
    }
}

/**
 * What to tell a person about a tracking domain. The DNS result is advice only: a name behind a proxy such as
 * Cloudflare never matches the record Thinpost looks for and still works. What decides is the secure connection.
 */
export function trackingStatus(view: TrackingView, sender: string, support: TrackingSupport): TrackingStatus {
    const name = view.name;
    const to = support.cnameTarget ?? 'the name shown below';
    const fallback = "New links use Thinpost's standard tracking address in the meantime. Nothing is lost.";
    if (!support.available) {
        // Preparation ignores tracking domains here, whatever the row's state says.
        return {
            label: 'Not in use',
            tone: 'warning',
            message: `Not in use: this installation doesn't use your own tracking address right now. ${name} is kept, and new links use Thinpost's standard tracking address. Ask your Thinpost administrator if you'd like it.`
        };
    }
    if (view.state === 'active') {
        const note = view.httpsResult && view.httpsResult !== 'ok' ? " The last check couldn't finish, so Thinpost will look again shortly." : '';
        return {
            label: 'In use',
            tone: 'success',
            message: `In use. Tracked links and the open-tracking image in mail from ${sender} use ${name}. One-click unsubscribe links keep using Thinpost's standard address.${note}`
        };
    }
    const failing = view.state === 'failing';
    const lead = failing ? 'Not in use. ' : '';
    if (!view.httpsResult) {
        return {
            label: 'Waiting for DNS record',
            tone: 'warning',
            message: `Add a CNAME record for ${name} pointing at ${to}, then choose Check now. Thinpost also checks on its own. ${fallback}`
        };
    }
    const dnsOk = view.dnsResult === 'ok';
    if (dnsOk || (view.httpsResult !== 'unreachable' && view.dnsResult !== 'missing')) {
        // The record is right, or the name answers some other way (a proxy): what is missing is the secure connection.
        const dns = dnsOk ? 'Your DNS record is right; your Thinpost administrator' : 'Your Thinpost administrator';
        return {
            label: failing ? 'Not in use' : 'Waiting for a secure connection',
            tone: 'warning',
            message: `${lead}Waiting for a secure connection. ${dns} still needs to finish setting up this address. ${connectionProblem(view)} Links will start using it on their own once it works. ${fallback}`
        };
    }
    if (view.dnsResult === 'missing') {
        return {
            label: failing ? 'Not in use' : 'Waiting for DNS record',
            tone: 'warning',
            message: `${lead}Thinpost can't find ${name} in DNS. Add a CNAME record for it pointing at ${to}. ${fallback}`
        };
    }
    if (view.dnsResult === 'wrong_target') {
        return {
            label: failing ? 'Not in use' : 'Waiting for DNS record',
            tone: 'warning',
            message: `${lead}${name} points at ${view.observedTarget ?? 'another name'}, not ${to}, and Thinpost couldn't reach it over a secure connection. Change the CNAME to point at ${to}. If a proxy such as Cloudflare is meant to be in front, ask your Thinpost administrator to check the address is set up. ${fallback}`
        };
    }
    return {
        label: failing ? 'Not in use' : 'Waiting for a secure connection',
        tone: 'warning',
        message: `${lead}Thinpost couldn't check ${name} just now. It will try again on its own. ${fallback}`
    };
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
