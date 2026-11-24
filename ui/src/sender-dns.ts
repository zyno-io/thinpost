import { InvalidResponseError } from './api-errors';
import type { SenderDomainDnsCheck } from './openapi-client-generated';

export type CheckTone = '' | 'success' | 'warning';

export interface CheckTag {
    label: string;
    tone: CheckTone;
}

/** ok: as is. problem: wrong, and shown highlighted. missing: absent, and shown as an insertion. */
export type PartState = 'ok' | 'problem' | 'missing';

export interface RecordPart {
    text: string;
    state: PartState;
    /** Names the issue of a problem or missing part. */
    hint?: string;
}

/** What the user should change: Update the current record, Add one, or keep only one of several. */
export type DmarcFix = { kind: 'update'; value: string; /** Set when applying it turns enforcement on. */ warning?: string } | { kind: 'add'; name: string; value: string } | { kind: 'keep-one'; text: string };

export interface DmarcView {
    tag: CheckTag;
    /** `_dmarc.<name>` of the record read. Absent when there is no record. */
    location?: string;
    /** The current record, tag by tag, plus any missing tag. Empty without a single record. */
    segments: RecordPart[];
    /** The records of a `multiple` status, each a problem. */
    records: string[];
    /** The record is cut: segments hold only its start, so nothing is flagged as missing and there is no fix. */
    truncated?: boolean;
    fix: DmarcFix | null;
}

export interface SpfView {
    result: CheckTag;
    /** Only when SPF passes. */
    alignment: (CheckTag & { title?: string }) | null;
    envelope: string;
    standard: boolean;
    note: string | null;
}

const MAX_NAME_LENGTH = 253;
const MAX_RECORD_LENGTH = 1024;
const MAX_RECORDS = 5;

const DMARC_STATUSES: readonly string[] = ['ok', 'monitoring', 'missing', 'multiple', 'invalid', 'unavailable'];
const POLICIES: readonly string[] = ['none', 'quarantine', 'reject'];
const ALIGNMENTS: readonly string[] = ['r', 's'];
const SPF_RESULTS: readonly string[] = ['pass', 'fail', 'softfail', 'neutral', 'none', 'permerror', 'temperror', 'no_sending_ip'];
const ENVELOPE_KINDS: readonly string[] = ['bounce_domain', 'standard'];

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value);
const isText = (value: unknown, max = MAX_NAME_LENGTH): value is string => typeof value === 'string' && new TextEncoder().encode(value).length <= max;
/** Absent, or a string within the limit. */
const isOptionalText = (value: unknown, max = MAX_NAME_LENGTH) => value === undefined || isText(value, max);
const isOptionalOneOf = (value: unknown, allowed: readonly string[]) => value === undefined || (typeof value === 'string' && allowed.includes(value));

/** Checks the response shape, and that it answers for `domain`, so a stale or foreign body never renders as a result. */
export function parseSenderDnsCheck(data: unknown, domain: string): SenderDomainDnsCheck {
    if (!isRecord(data) || data.domain !== domain || !isRecord(data.dmarc) || !isRecord(data.spf)) throw new InvalidResponseError();
    const { dmarc, spf } = data;
    if (
        typeof dmarc.status !== 'string' ||
        !DMARC_STATUSES.includes(dmarc.status) ||
        !isText(dmarc['org_domain']) ||
        !isOptionalText(dmarc.record_domain) ||
        !isOptionalText(dmarc.record, MAX_RECORD_LENGTH) ||
        !(dmarc.record_truncated === undefined || typeof dmarc.record_truncated === 'boolean') ||
        !isOptionalOneOf(dmarc.policy, POLICIES) ||
        !isOptionalOneOf(dmarc.aspf, ALIGNMENTS) ||
        !(dmarc.records === undefined || (Array.isArray(dmarc.records) && dmarc.records.length <= MAX_RECORDS && dmarc.records.every(r => isText(r, MAX_RECORD_LENGTH))))
    ) {
        throw new InvalidResponseError();
    }
    if (typeof spf.result !== 'string' || !SPF_RESULTS.includes(spf.result) || typeof spf.aligned !== 'boolean' || !isOptionalText(spf.envelope_domain) || !isOptionalOneOf(spf.envelope_kind, ENVELOPE_KINDS)) {
        throw new InvalidResponseError();
    }
    if (spf.result !== 'no_sending_ip' && (!spf.envelope_domain || !spf.envelope_kind)) throw new InvalidResponseError();
    return data as unknown as SenderDomainDnsCheck;
}

const POLICY_HINT = 'p must be none, quarantine or reject';
const ADD_RECORD = 'v=DMARC1; p=none';

interface Draft extends RecordPart {
    /** What the corrected record has in place of this part; null drops it. */
    replacement: string | null;
}

/** Reads a record as RFC 7489 6.3 does, and as the server's parseDMARC does. */
function draftParts(record: string, truncated: boolean): Draft[] {
    const tags = record
        .split(';')
        .map(tag => tag.trim())
        .filter(tag => tag !== '');
    const drafts: Draft[] = [];
    const valid = (value: string, allowed: readonly string[]) => allowed.includes(value.toLowerCase());
    tags.forEach((text, i) => {
        const eq = text.indexOf('=');
        const key = (eq < 0 ? text : text.slice(0, eq)).trim().toLowerCase();
        const value = eq < 0 ? '' : text.slice(eq + 1).trim();
        const ok = (): Draft => ({ text, state: 'ok', replacement: text });
        const drop = (hint: string): Draft => ({ text, state: 'problem', hint, replacement: null });
        if (i === 0) {
            drafts.push(eq >= 0 && key === 'v' && value === 'DMARC1' ? ok() : { text, state: 'problem', hint: 'Must start with v=DMARC1', replacement: 'v=DMARC1' });
        } else if (eq < 0) {
            drafts.push(drop('Not a tag=value pair'));
        } else if (key === 'p' || key === 'sp') {
            drafts.push(valid(value, POLICIES) ? ok() : key === 'p' ? { text, state: 'problem', hint: POLICY_HINT, replacement: 'p=none' } : drop('sp must be none, quarantine or reject'));
        } else if (key === 'aspf' || key === 'adkim') {
            drafts.push(valid(value, ALIGNMENTS) ? ok() : drop(`${key} must be r or s`));
        } else if (key === 'pct') {
            drafts.push(/^\d+$/.test(value) && Number(value) <= 100 ? ok() : drop('pct must be a number from 0 to 100'));
        } else {
            drafts.push(ok());
        }
    });
    if (truncated) {
        // The last tag may be cut mid-way, and p may be past the cut: neither is a finding.
        const last = drafts[drafts.length - 1];
        if (last) Object.assign(last, { state: 'ok', replacement: last.text, hint: undefined });
        return drafts;
    }
    const hasValidP = drafts.some((d, i) => i > 0 && d.state === 'ok' && /^p\s*=/i.test(d.text));
    let converted = false;
    for (const d of drafts) {
        if (d.state === 'problem' && d.hint === POLICY_HINT) {
            if (hasValidP || converted) d.replacement = null;
            converted = true;
        }
    }
    if (!hasValidP && !converted) {
        drafts.splice(Math.min(1, drafts.length), 0, { text: 'p=none', state: 'missing', hint: 'p is missing', replacement: 'p=none' });
    }
    return drafts;
}

const DMARC_TAGS: Record<string, CheckTag> = {
    ok: { label: 'Enforced', tone: 'success' },
    monitoring: { label: 'Monitoring', tone: 'success' },
    missing: { label: 'Missing', tone: 'warning' },
    invalid: { label: 'Not valid', tone: 'warning' },
    multiple: { label: 'Duplicate records', tone: 'warning' },
    unavailable: { label: "Couldn't check", tone: '' }
};

/** What receivers find for the domain's DMARC policy, as a record with its problems marked and the fix. */
export function dmarcView(check: SenderDomainDnsCheck): DmarcView {
    const { dmarc, domain } = check;
    const tag = DMARC_TAGS[dmarc.status] ?? DMARC_TAGS['unavailable']!;
    const view: DmarcView = { tag, segments: [], records: [], fix: null };
    if (dmarc.status === 'unavailable') return view;
    if (dmarc.status === 'missing') {
        view.fix = { kind: 'add', name: `TXT _dmarc.${dmarc['org_domain']}`, value: ADD_RECORD };
        return view;
    }
    view.location = `_dmarc.${dmarc.record_domain ?? domain}`;
    if (dmarc.status === 'multiple') {
        view.records = dmarc.records ?? [];
        view.fix = { kind: 'keep-one', text: 'Keep one; delete the others.' };
        return view;
    }
    if (!dmarc.record) return view;
    const truncated = dmarc.record_truncated === true;
    const drafts = draftParts(dmarc.record, truncated);
    view.segments = drafts.map(({ text, state, hint }) => ({ text, state, ...(hint ? { hint } : {}) }));
    if (truncated) {
        view.truncated = true;
        return view;
    }
    if (drafts.some(d => d.state !== 'ok')) {
        const tags = drafts.map(d => d.replacement).filter((t): t is string => t !== null);
        const fix: DmarcFix = { kind: 'update', value: tags.join('; ') };
        const turnedOn = enforcementTurnedOn(check, tags);
        if (turnedOn) fix.warning = `Applying this turns on p=${turnedOn}.`;
        view.fix = fix;
    }
    return view;
}

const RANK: Record<string, number> = { none: 0, quarantine: 1, reject: 2 };

/** The policy the fixed record newly enforces for this domain, if it is stricter than what receivers apply now. */
function enforcementTurnedOn(check: SenderDomainDnsCheck, tags: string[]): string | null {
    const { dmarc, domain } = check;
    const last = (name: string) => {
        let found: string | undefined;
        for (const tag of tags.slice(1)) {
            const eq = tag.indexOf('=');
            if (eq >= 0 && tag.slice(0, eq).trim().toLowerCase() === name) found = tag.slice(eq + 1).trim().toLowerCase();
        }
        return found;
    };
    const inherited = (dmarc.record_domain ?? domain) !== domain;
    const fixed = (inherited ? last('sp') : undefined) ?? last('p') ?? 'none';
    // ok: the server's policy. monitoring and invalid: none, or no DMARC at all.
    const current = dmarc.status === 'ok' ? (dmarc.policy ?? 'none') : 'none';
    return (RANK[fixed] ?? 0) > (RANK[current] ?? 0) ? fixed : null;
}

const STRICT_TITLE = 'Your DMARC record asks for strict SPF (aspf=s). DKIM aligns instead.';
const RELAXED_TITLE = 'DKIM aligns instead. A bounce domain aligns SPF too.';

/** SPF for the IP this app sends from, at the envelope domain its mail uses. */
export function spfView(check: SenderDomainDnsCheck): SpfView {
    const { spf, domain, dmarc } = check;
    const view: SpfView = { result: { label: 'Fails', tone: 'warning' }, alignment: null, envelope: spf.envelope_domain ?? '', standard: spf.envelope_kind === 'standard', note: null };
    switch (spf.result) {
        case 'pass':
            view.result = { label: 'Passes', tone: 'success' };
            view.alignment = spf.aligned ? { label: 'Aligned', tone: 'success' } : { label: 'Not aligned', tone: '', title: dmarc.aspf === 's' ? STRICT_TITLE : RELAXED_TITLE };
            break;
        case 'temperror':
            view.result = { label: "Couldn't check", tone: '' };
            break;
        case 'no_sending_ip':
            view.result = { label: "Couldn't check", tone: '' };
            view.note = 'No sending IP reported yet.';
            break;
        default:
            view.note = `Set by your Thinpost administrator, not on ${domain}.`;
    }
    return view;
}

/** The one-word result next to the Check button: what needs attention, then what couldn't be checked. */
export function senderDnsSummary(check: SenderDomainDnsCheck): CheckTag {
    const dmarc = dmarcView(check);
    const spf = spfView(check);
    if (['missing', 'invalid', 'multiple'].includes(check.dmarc.status) || dmarc.fix || spf.result.tone === 'warning') return { label: 'Needs attention', tone: 'warning' };
    if (dmarc.tag.tone === '' || spf.result.tone === '') return { label: "Couldn't check everything", tone: '' };
    return { label: 'All good', tone: 'success' };
}
