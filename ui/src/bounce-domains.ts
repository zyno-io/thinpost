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
    lastOkAt: string | null;
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
        lastOkAt: text(row.last_ok_at),
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

export interface StatusTag {
    label: string;
    tone: Tone;
}

/** The record as the last check found it, when that differs from what is wanted: wrong (problem) or absent (missing). */
export interface ObservedPart {
    state: 'problem' | 'missing';
    text: string;
    hint: string;
}

/** What to do at the DNS provider: Add the CNAME, or Update the one that is there. */
export interface CnameFix {
    kind: 'add' | 'update';
    host: string;
    value: string;
}

/** The Record row of a bounce or tracking domain: the CNAME wanted, what was found instead, and the fix. */
export interface CnameRecordView {
    name: string;
    /** What the CNAME must point at; null when the API has not confirmed it. */
    target: string | null;
    observed: ObservedPart | null;
    fix: CnameFix | null;
}

export interface BounceRecordView {
    tag: StatusTag;
    record: CnameRecordView;
    /** One short line, only when it says something to act on. */
    note: string | null;
}

export const DNS_ONLY_NOTE = 'DNS only, not proxied.';
const NOT_CNAME_HINT = "Exists but isn't a CNAME. A proxied record looks like this.";

/** The note for a name to point at that the API has not confirmed. */
export function noTargetNote(target: CnameTarget): string {
    return target.status === 'dns_unconfirmed' ? "Can't confirm the name to point at. Ask your Thinpost administrator." : 'Name to point at not known yet. Ask your Thinpost administrator.';
}

/** How recent a passing check must be for mail to use a bounce domain (the server's customdomain.Freshness). */
export const FRESHNESS_MS = 3 * 60 * 60 * 1000;

export const MATCHES_NOTE = 'Matches now. Choose Check now.';
export const RETRY_NOTE = "Last check didn't finish. Thinpost will retry.";
export const LOOKUP_NOTE = "Couldn't check DNS. Thinpost will retry.";

/** Why an active bounce domain is not being used: the record moved off the wanted name, or the last pass is too old. */
function activeProblem(view: BounceView, point: string | null, now: number): 'moved' | 'stale' | null {
    if (view.state !== 'active') return null;
    if (point && view.observedTarget && view.observedTarget !== point) return 'moved';
    if (view.lastOkAt) {
        const passed = Date.parse(view.lastOkAt);
        if (Number.isFinite(passed) && now - passed >= FRESHNESS_MS) return 'stale';
    }
    return null;
}

/** The short tag for a bounce domain. */
export function bounceStatus(view: BounceView, target: CnameTarget, now: number = Date.now()): StatusTag {
    if (view.state === 'active') return activeProblem(view, target.target, now) ? { label: 'Not in use', tone: 'warning' } : { label: 'In use', tone: 'success' };
    const failing = view.state === 'failing';
    const waiting: StatusTag = { label: failing ? 'Not in use' : 'Waiting for DNS record', tone: 'warning' };
    if (!target.target) return waiting;
    switch (view.result) {
        case 'missing':
        case 'not_cname':
        case 'other_ip':
        case 'wrong_target':
            return { label: 'Not in use', tone: 'warning' };
        default:
            return waiting;
    }
}

/**
 * What the Record row of a bounce domain shows. The copy is for app developers and customer service: the record,
 * what was found instead, and the one line to act on. Why a bounce domain helps is in the help text.
 */
export function bounceView(view: BounceView, target: CnameTarget, now: number = Date.now()): BounceRecordView {
    const tag = bounceStatus(view, target, now);
    const point = target.target;
    const record: CnameRecordView = { name: view.name, target: point, observed: null, fix: null };
    const update = (text: string, hint: string): ObservedPart => ({ state: 'problem', text, hint });
    // The last result is old news: the record has since been set to what is wanted.
    if (point && view.observedTarget === point && (view.result === 'other_ip' || view.result === 'wrong_target')) {
        return { tag, record, note: MATCHES_NOTE };
    }
    if (view.state === 'active') {
        const problem = activeProblem(view, point, now);
        if (problem === 'moved' && point && view.observedTarget) {
            record.observed = update(view.observedTarget, 'Points at the wrong name');
            record.fix = { kind: 'update', host: view.name, value: point };
            return { tag, record, note: DNS_ONLY_NOTE };
        }
        const unfinished = problem === 'stale' || (view.result && view.result !== 'ok');
        return { tag, record, note: unfinished ? RETRY_NOTE : null };
    }
    if (!point) return { tag, record, note: noTargetNote(target) };
    switch (view.result) {
        case 'lookup_failed':
        case 'no_sending_ip':
            // Never passed, so the record is still the thing to add; the check just could not say.
            if (view.observedTarget !== point) {
                record.fix = { kind: view.observedTarget ? 'update' : 'add', host: view.name, value: point };
            }
            return { tag, record, note: LOOKUP_NOTE };
        case 'not_cname':
            record.observed = update('not a CNAME', NOT_CNAME_HINT);
            record.fix = { kind: 'update', host: view.name, value: point };
            return { tag, record, note: 'Turn off any proxy (orange cloud).' };
        case 'other_ip':
            record.observed = update(view.observedTarget ?? 'another name', 'Points at the name of a different sending IP');
            record.fix = { kind: 'update', host: view.name, value: point };
            return { tag, record, note: 'This app now sends from a different IP.' };
        case 'wrong_target':
            record.observed = update(view.observedTarget ?? 'another name', 'Points at the wrong name');
            record.fix = { kind: 'update', host: view.name, value: point };
            return { tag, record, note: DNS_ONLY_NOTE };
        default:
            // missing, or not checked yet.
            record.observed = { state: 'missing', text: point, hint: view.result === 'missing' ? 'Not found in DNS' : 'Not added yet' };
            record.fix = { kind: 'add', host: view.name, value: point };
            return { tag, record, note: DNS_ONLY_NOTE };
    }
}

/**
 * The nearest parent of a sending domain (in the app's own list) that has its own bounce or tracking domain in use,
 * as the server does when it picks one for subdomain mail. `pick` returns that domain's name, or null.
 */
export function inheritedFrom<T extends { name: string }>(sender: string, domains: readonly T[], pick: (d: T) => string | null): { from: string; name: string } | null {
    const byName = new Map(domains.map(d => [d.name, d]));
    for (let rest = sender.split('.').slice(1); rest.length > 1; rest = rest.slice(1)) {
        const parent = byName.get(rest.join('.'));
        const name = parent ? pick(parent) : null;
        if (parent && name) return { from: parent.name, name };
    }
    return null;
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
