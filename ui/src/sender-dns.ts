import { InvalidResponseError } from './api-errors';
import type { SenderDomainDnsCheck } from './openapi-client-generated';

export type CheckTone = '' | 'success' | 'warning';

export interface CheckLine {
    label: string;
    tone: CheckTone;
    message: string;
}

const MAX_NAME_LENGTH = 253;
const MAX_RECORD_LENGTH = 1024;

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
        !isOptionalOneOf(dmarc.policy, POLICIES) ||
        !isOptionalOneOf(dmarc.aspf, ALIGNMENTS)
    ) {
        throw new InvalidResponseError();
    }
    if (typeof spf.result !== 'string' || !SPF_RESULTS.includes(spf.result) || typeof spf.aligned !== 'boolean' || !isOptionalText(spf.envelope_domain) || !isOptionalOneOf(spf.envelope_kind, ENVELOPE_KINDS)) {
        throw new InvalidResponseError();
    }
    if (spf.result !== 'no_sending_ip' && (!spf.envelope_domain || !spf.envelope_kind)) throw new InvalidResponseError();
    return data as unknown as SenderDomainDnsCheck;
}

/** What receivers find for the domain's DMARC policy. */
export function dmarcLine(check: SenderDomainDnsCheck): CheckLine {
    const { dmarc, domain } = check;
    const at = dmarc.record_domain ?? domain;
    const inherited = at !== domain ? ` The record is the one at _dmarc.${at}, which covers its subdomains.` : '';
    const passes = ' Mail sent through Thinpost passes, because DKIM signs it with your domain.';
    switch (dmarc.status) {
        case 'ok':
            return {
                label: 'Enforced',
                tone: 'success',
                message: `DMARC is set up: receivers are asked to ${dmarc.policy === 'reject' ? 'reject' : 'treat as spam'} mail from ${domain} that fails it.${inherited}${passes}`
            };
        case 'monitoring':
            return {
                label: 'Monitoring only',
                tone: 'success',
                message: `DMARC is set up to monitor only (p=none): receivers report mail from ${domain} that fails it but still deliver it.${inherited}${passes} When the reports show all your mail passes, you can move to p=quarantine or p=reject.`
            };
        case 'missing': {
            const org = dmarc['org_domain'];
            return {
                label: 'Not found',
                tone: 'warning',
                message: `There's no DMARC record for ${domain}${org !== domain ? ` or ${org}` : ''}. Large mailbox providers expect one, and require it for bulk mail. Add a TXT record at _dmarc.${org}, for example "v=DMARC1; p=none", with rua= and an address of yours if you want reports.`
            };
        }
        case 'multiple':
            return { label: 'More than one', tone: 'warning', message: `There's more than one DMARC record at _dmarc.${at}, so receivers ignore them all. Keep exactly one.` };
        case 'invalid':
            return {
                label: 'Not valid',
                tone: 'warning',
                message: `The DMARC record at _dmarc.${at} isn't valid, so receivers ignore it. It needs p=none, p=quarantine or p=reject (or an rua= reporting address, which counts as p=none).`
            };
        default:
            return { label: "Couldn't check", tone: '', message: "Thinpost couldn't look up DMARC just now. Try again in a moment." };
    }
}

const SPF_REASONS: Record<string, string> = {
    none: "there's no SPF record",
    fail: "the SPF record doesn't list the IP this app sends from",
    softfail: "the SPF record doesn't list the IP this app sends from",
    neutral: "the SPF record doesn't say whether the IP this app sends from may send",
    permerror: 'the SPF record has an error, or needs more than 10 DNS lookups'
};

/** SPF for the IP this app sends from, at the envelope domain its mail uses. */
export function spfLine(check: SenderDomainDnsCheck): CheckLine {
    const { spf, domain, dmarc } = check;
    const env = spf.envelope_domain ?? '';
    switch (spf.result) {
        case 'pass':
            if (spf.aligned) {
                return { label: 'Passes', tone: 'success', message: `SPF passes for ${env}${spf.envelope_kind === 'bounce_domain' ? ', the bounce domain in use,' : ''} and lines up with DMARC.` };
            }
            if (dmarc.aspf === 's') {
                return { label: 'Passes', tone: 'success', message: `SPF passes for ${env}, but your DMARC record asks for strict SPF (aspf=s), so it doesn't line up. That's fine: DKIM lines up instead.` };
            }
            return {
                label: 'Passes',
                tone: 'success',
                message: `SPF passes for ${env}, Thinpost's standard bounce address. It isn't your domain, so SPF doesn't line up with DMARC. That's fine: DKIM lines up instead. To have SPF line up too, use a bounce domain of yours (see above).`
            };
        case 'temperror':
            return { label: "Couldn't check", tone: '', message: "Thinpost couldn't look up SPF just now. Try again in a moment." };
        case 'no_sending_ip':
            return {
                label: "Couldn't check",
                tone: '',
                message: "Thinpost doesn't know which IP this app sends from yet, because the sending worker hasn't reported in. Ask your Thinpost administrator."
            };
        default:
            return {
                label: "Doesn't pass",
                tone: 'warning',
                message: `SPF doesn't pass for ${env}: ${SPF_REASONS[spf.result] ?? SPF_REASONS.permerror}. This record is set up by your Thinpost administrator for the IP this app sends from, so ask them to fix it. You don't need to change ${domain}'s own SPF record, and DMARC still passes through DKIM.`
            };
    }
}

/** The one-word result next to the Check button: warnings first, then what couldn't be checked. */
export function senderDnsSummary(check: SenderDomainDnsCheck): { text: string; tone: CheckTone } {
    const tones = [dmarcLine(check).tone, spfLine(check).tone];
    if (tones.includes('warning')) return { text: 'Needs attention', tone: 'warning' };
    if (tones.includes('')) return { text: "Couldn't check everything", tone: '' };
    return { text: 'All good', tone: 'success' };
}
