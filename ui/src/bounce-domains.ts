import { normalizeDomain } from './validation';

/** The longest bounce domain Thinpost can put in a Return-Path. */
export const MAX_BOUNCE_DOMAIN_LENGTH = 219;

export type BounceState = 'pending' | 'active' | 'failing';
export type BounceResult = 'ok' | 'missing' | 'not_cname' | 'other_ip' | 'wrong_target' | 'lookup_failed' | 'no_sending_ip';
export type CnameTargetStatus = 'ok' | 'not_reported' | 'dns_unconfirmed';

/** A bounce domain as the API sends it, read tolerantly. */
export interface BounceView {
    name: string;
    state: BounceState;
    checking: boolean;
    result: BounceResult | null;
    observedTarget: string | null;
    checkedAt: string | null;
    verifiedAt: string | null;
}

const STATES: readonly string[] = ['pending', 'active', 'failing'];
const RESULTS: readonly string[] = ['ok', 'missing', 'not_cname', 'other_ip', 'wrong_target', 'lookup_failed', 'no_sending_ip'];

const text = (value: unknown): string | null => (typeof value === 'string' && value ? value : null);

/** Reads a bounce domain; null when it is absent or malformed, so a bad body never renders as a state. */
export function parseBounceDomain(value: unknown): BounceView | null {
    if (typeof value !== 'object' || value === null) return null;
    const row = value as Record<string, unknown>;
    const name = text(row.name);
    if (!name || typeof row.state !== 'string' || !STATES.includes(row.state)) return null;
    return {
        name,
        state: row.state as BounceState,
        checking: row.checking === true,
        result: typeof row.result === 'string' && RESULTS.includes(row.result) ? (row.result as BounceResult) : null,
        observedTarget: text(row.observed_target),
        checkedAt: text(row.checked_at),
        verifiedAt: text(row.verified_at)
    };
}

/** What a bounce domain's CNAME must point at, as the API reports it. */
export interface CnameTarget {
    status: CnameTargetStatus;
    target: string | null;
}

export function parseCnameTarget(domain: { cname_target?: unknown; cname_target_status?: unknown }): CnameTarget {
    const status: CnameTargetStatus = domain.cname_target_status === 'ok' || domain.cname_target_status === 'dns_unconfirmed' ? domain.cname_target_status : 'not_reported';
    const target = status === 'ok' ? text(domain.cname_target) : null;
    // A confirmed status without a name is not a usable answer.
    return target ? { status, target } : { status: status === 'ok' ? 'not_reported' : status, target: null };
}

export type Tone = 'success' | 'warning' | 'danger';

export interface BounceStatus {
    label: string;
    tone: Tone;
    /** Plain-language explanation for app developers and support staff. */
    message: string;
}

const DNS_ONLY = 'DNS only, not proxied';

/** Why the CNAME's name is not shown, when it is not. */
function noTargetMessage(target: CnameTarget): string {
    return target.status === 'dns_unconfirmed'
        ? "Thinpost can't confirm the name to point at in DNS right now. Ask your Thinpost administrator."
        : "Thinpost doesn't know the name to point at yet, because the sending worker hasn't reported in. Ask your Thinpost administrator.";
}

/**
 * What to tell a person about a bounce domain. `sender` is the sending domain it belongs to; `target` is the
 * name the CNAME must use. The copy is for app developers and customer service, so it says what the DNS record
 * should be and what mail does meanwhile, never how Thinpost decides.
 */
export function bounceStatus(view: BounceView, sender: string, target: CnameTarget): BounceStatus {
    const name = view.name;
    const point = target.target;
    const to = point ?? 'the name shown below';
    const fallback = "Mail is using Thinpost's standard bounce address in the meantime. Nothing is lost.";
    if (view.state === 'active') {
        const note = view.result && view.result !== 'ok' ? " The last check couldn't finish, so Thinpost will look again shortly." : '';
        return {
            label: 'In use',
            tone: 'success',
            message: `In use. Bounces for mail from ${sender} come back to ${name}, so SPF passes for your domain too. This lines up with DMARC's usual check. If your DMARC record asks for strict SPF (aspf=s), DKIM still carries it.${note}`
        };
    }
    if (!point) {
        return { label: view.state === 'failing' ? 'Not in use' : 'Waiting for DNS record', tone: 'warning', message: `${noTargetMessage(target)} ${fallback}` };
    }
    switch (view.result) {
        case 'missing':
            return { label: 'Not in use', tone: 'warning', message: `Not in use. Thinpost can't find ${name} in DNS. Add a CNAME record for it pointing at ${to} (${DNS_ONLY}). ${fallback}` };
        case 'not_cname':
            return {
                label: 'Not in use',
                tone: 'warning',
                message: `Not in use. ${name} exists but isn't a CNAME. Replace it with a CNAME pointing at ${to}. If your DNS provider proxies records (an orange cloud), turn that off: this record must be ${DNS_ONLY}. ${fallback}`
            };
        case 'other_ip':
            return { label: 'Not in use', tone: 'warning', message: `This app now sends from a different IP. Point the CNAME at ${to}. ${fallback}` };
        case 'wrong_target':
            return {
                label: 'Not in use',
                tone: 'warning',
                message: `Not in use. ${name} points at ${view.observedTarget ?? 'another name'}, not ${to}. Change the CNAME to point at ${to} (${DNS_ONLY}). ${fallback}`
            };
        case 'lookup_failed':
        case 'no_sending_ip':
            return { label: view.state === 'failing' ? 'Not in use' : 'Waiting for DNS record', tone: 'warning', message: `Thinpost couldn't check DNS just now. It will try again on its own. ${fallback}` };
        default:
            if (view.state === 'failing') {
                return { label: 'Not in use', tone: 'warning', message: `Not in use. Check the DNS record. The CNAME is missing or points somewhere else. ${fallback}` };
            }
            return { label: 'Waiting for DNS record', tone: 'warning', message: `Add a CNAME record for ${name} pointing at ${to} (${DNS_ONLY}), then choose Check now. ${fallback}` };
    }
}

export type BounceNameInput = { name: string; error: null } | { name: null; error: string };

/** Mirrors the API's rules: a strict subdomain of the sending domain, at most 219 characters. The server decides. */
export function validateBounceName(value: string, sender: string): BounceNameInput {
    const parsed = normalizeDomain(value);
    if (parsed.domain === null) return { name: null, error: parsed.error };
    if (parsed.domain.length > MAX_BOUNCE_DOMAIN_LENGTH) return { name: null, error: `Use at most ${MAX_BOUNCE_DOMAIN_LENGTH} characters.` };
    if (!parsed.domain.endsWith(`.${sender}`) || parsed.domain.length <= sender.length + 1) {
        return { name: null, error: `Use a subdomain of ${sender}, such as bounces.${sender}.` };
    }
    return { name: parsed.domain, error: null };
}

/** The name suggested for a sending domain. */
export const suggestBounceName = (sender: string) => `bounces.${sender}`;
