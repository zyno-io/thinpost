import { InvalidResponseError } from './api-errors';
import type { FeedbackLoops } from './openapi-client-generated';

/** The reporter allowlist holds at most this many domains (server limit). */
export const MAX_REPORTERS = 32;

export const FEEDBACK_RESULTS = ['accepted', 'duplicate', 'ignored', 'unauthenticated', 'unparsable', 'uncorrelated', 'not_delivered'] as const;
export type FeedbackResult = (typeof FEEDBACK_RESULTS)[number];

/** What each counted result means and what an operator should do about a growing count. */
export const FEEDBACK_RESULT_INFO: Readonly<Record<FeedbackResult, { label: string; help: string }>> = {
    accepted: { label: 'Complaints recorded', help: 'A new complaint. The recipient was added to the suppression list and a complained event was sent.' },
    duplicate: { label: 'Repeats', help: 'A report for a delivery that already has a complaint. Nothing new was recorded.' },
    ignored: { label: 'Not abuse reports', help: 'A report that isn\'t about abuse (for example not-spam or fraud).' },
    unauthenticated: { label: 'Unauthenticated', help: 'No valid signature from a listed reporter domain. A steady count usually means the reporter domain list is wrong or empty.' },
    unparsable: { label: 'Unparsable', help: 'Not a valid abuse report, or too large to read.' },
    uncorrelated: { label: 'Not matched', help: 'The original message inside the report doesn\'t match a Thinpost delivery. A steady count usually means the provider removes or changes details of the original.' },
    not_delivered: { label: 'Not delivered', help: 'The matching delivery wasn\'t accepted by the recipient\'s mail server, so no complaint applies.' }
};

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value);
const isTime = (value: unknown): value is string => typeof value === 'string' && value.length <= 64 && !Number.isNaN(new Date(value).getTime());

/** Checks the response shape so a stale or foreign body never renders as an empty state. */
export function parseFeedbackLoops(data: unknown): FeedbackLoops {
    if (!isRecord(data) || typeof data.receiving !== 'boolean' || !Array.isArray(data.reporters) || !Array.isArray(data.counts)) throw new InvalidResponseError();
    if (data.reporters.length > MAX_REPORTERS || data.reporters.some(d => typeof d !== 'string' || d.length > 253)) throw new InvalidResponseError();
    for (const c of data.counts) {
        if (
            !isRecord(c) ||
            typeof c.day !== 'string' ||
            !/^\d{4}-\d{2}-\d{2}$/.test(c.day) ||
            (c.channel !== 'arf' && c.channel !== 'one_click') ||
            !FEEDBACK_RESULTS.includes(c.result as FeedbackResult) ||
            typeof c.count !== 'number' ||
            !Number.isInteger(c.count) ||
            c.count < 0
        ) {
            throw new InvalidResponseError();
        }
    }
    if (data.mailbox !== undefined) {
        const m = data.mailbox;
        if (!isRecord(m) || !isTime(m.created_at) || (m.address !== undefined && (typeof m.address !== 'string' || m.address.length > 300))) throw new InvalidResponseError();
    }
    return data as unknown as FeedbackLoops;
}

export interface CountRow {
    result: FeedbackResult;
    today: number;
    total: number;
}

/** Complaint-feed counts per result for the last 14 days (the server's window); `today` is the newest day returned, only when it is the UTC day of `now`. */
export function countRows(counts: FeedbackLoops['counts'], now: Date): CountRow[] {
    const today = now.toISOString().slice(0, 10);
    return FEEDBACK_RESULTS.map(result => {
        const mine = counts.filter(c => c.channel === 'arf' && c.result === result);
        return { result, today: mine.filter(c => c.day === today).reduce((n, c) => n + c.count, 0), total: mine.reduce((n, c) => n + c.count, 0) };
    });
}

const DOMAIN_LABEL = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

export interface ReporterParse {
    domains: string[];
    problem: string | null;
}

/** One domain per line (or separated by commas or spaces), lowercased and de-duplicated, validated like the server. */
export function parseReporterDomains(text: string): ReporterParse {
    const domains: string[] = [];
    for (const raw of text.split(/[\s,]+/)) {
        const domain = raw.trim().toLowerCase().replace(/\.$/, '');
        if (!domain) continue;
        const labels = domain.split('.');
        if (domain.length > 253 || labels.length < 2 || !labels.every(l => DOMAIN_LABEL.test(l)) || /^\d+(\.\d+)*$/.test(domain)) {
            return { domains, problem: 'Each entry must be a domain name such as arf.mail.yahoo.com. Use plain letters and digits; for international names, use the xn-- form.' };
        }
        if (!domains.includes(domain)) domains.push(domain);
    }
    if (domains.length > MAX_REPORTERS) return { domains, problem: `At most ${MAX_REPORTERS} domains.` };
    return { domains, problem: null };
}
