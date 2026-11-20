import { InvalidResponseError } from './api-errors';
import type { AppSmtp, SmtpCredential, SmtpFromPolicy, SmtpSettings } from './openapi-client-generated';

/** Mirrors the server: an app can hold at most this many active SMTP credentials. */
export const MAX_ACTIVE_CREDENTIALS = 10;

/** The SMTP submission ports, in the order the console recommends them. */
export const SMTP_PORTS = [
    { port: 465, security: 'tls', label: 'Port 465', note: 'Encrypted from the first byte (SSL/TLS). Recommended.' },
    { port: 587, security: 'starttls', label: 'Port 587', note: 'Starts plain and upgrades with STARTTLS. Set your software to require STARTTLS.' }
] as const;

export type FromMode = SmtpFromPolicy['mode'];

const DOT_ATOM = /^[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+(\.[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+)*$/;
const MAX_LOCAL_PART_BYTES = 64;
const MAX_ADDRESS_BYTES = 254;

const isRecord = (value: unknown): value is Record<string, unknown> => !!value && typeof value === 'object' && !Array.isArray(value);

function readCredential(raw: unknown): SmtpCredential {
    if (!isRecord(raw)) throw new InvalidResponseError();
    const { id, name, username, active, created_at: createdAt } = raw;
    if (typeof id !== 'string' || typeof name !== 'string' || typeof username !== 'string' || typeof active !== 'boolean' || typeof createdAt !== 'string') {
        throw new InvalidResponseError();
    }
    const out: SmtpCredential = { id, name, username, active, created_at: createdAt };
    if (typeof raw.revoked_at === 'string') out.revoked_at = raw.revoked_at;
    if (typeof raw.last_used_at === 'string') out.last_used_at = raw.last_used_at;
    return out;
}

function readPolicy(raw: unknown): SmtpFromPolicy {
    if (!isRecord(raw)) throw new InvalidResponseError();
    const { mode, only_unauthorized: onlyUnauthorized } = raw;
    if ((mode !== 'off' && mode !== 'domain' && mode !== 'address') || typeof onlyUnauthorized !== 'boolean') {
        throw new InvalidResponseError();
    }
    const out: SmtpFromPolicy = { mode, only_unauthorized: onlyUnauthorized };
    if (typeof raw.domain === 'string') out.domain = raw.domain;
    if (typeof raw.address === 'string') out.address = raw.address;
    return out;
}

/** Validates the settings object of getAppSmtp and updateAppSmtpSettings. */
export function parseSmtpSettings(data: unknown): SmtpSettings {
    if (!isRecord(data)) throw new InvalidResponseError();
    let signingReady: SmtpSettings['signing_ready'] = null;
    if (Array.isArray(data.signing_ready)) {
        signingReady = data.signing_ready.map(entry => {
            if (!isRecord(entry) || typeof entry.domain !== 'string' || typeof entry.ready !== 'boolean') throw new InvalidResponseError();
            return { domain: entry.domain, ready: entry.ready };
        });
    } else if (data.signing_ready !== null && data.signing_ready !== undefined) {
        throw new InvalidResponseError();
    }
    return { from: readPolicy(data.from), signing_ready: signingReady };
}

/** Validates the getAppSmtp response and keeps only contract fields. */
export function parseAppSmtp(data: unknown): AppSmtp {
    if (!isRecord(data) || !Array.isArray(data.credentials) || !isRecord(data.connection)) throw new InvalidResponseError();
    const connection = data.connection;
    if (typeof connection.primary_unnamed !== 'boolean') throw new InvalidResponseError();
    const hostSource = connection.host_source;
    if (hostSource !== null && hostSource !== 'app_sending_ip' && hostSource !== 'primary') throw new InvalidResponseError();
    const observed = Array.isArray(connection.observed_ports) ? connection.observed_ports.filter((p): p is number => Number.isInteger(p)) : [];
    return {
        settings: parseSmtpSettings(data.settings),
        credentials: data.credentials.map(readCredential),
        connection: {
            ...(typeof connection.host === 'string' && connection.host ? { host: connection.host } : {}),
            host_source: hostSource,
            primary_unnamed: connection.primary_unnamed,
            ports: [
                { port: 465, security: 'tls' },
                { port: 587, security: 'starttls' }
            ],
            observed_ports: observed,
            ...(typeof connection.observed_at === 'string' ? { observed_at: connection.observed_at } : {})
        }
    };
}

/** The credential and one-time password from createAppSmtpCredential; null when the response lacks either. */
export function parseIssuedCredential(data: unknown): { name: string; username: string; password: string } | null {
    if (!isRecord(data) || !isRecord(data.credential)) return null;
    const { username, password } = data;
    const name = data.credential.name;
    if (typeof username !== 'string' || !username || typeof password !== 'string' || !password || typeof name !== 'string') return null;
    return { name, username, password };
}

export type PortAvailability = 'listening' | 'not-listening' | 'unknown';

/** What the Edge last reported for a port; an availability hint only. Unknown until the Edge has reported. */
export function portAvailability(observedPorts: readonly number[], observedAt: string | undefined, port: number): PortAvailability {
    if (!observedAt) return 'unknown';
    return observedPorts.includes(port) ? 'listening' : 'not-listening';
}

/** Draft of the From-address form. Fields of other modes are kept so switching back loses nothing. */
export interface FromForm {
    mode: FromMode;
    domain: string;
    address: string;
    onlyUnauthorized: boolean;
}

export function formFromPolicy(policy: SmtpFromPolicy): FromForm {
    return {
        mode: policy.mode,
        domain: policy.domain ?? '',
        address: policy.address ?? '',
        onlyUnauthorized: policy.only_unauthorized
    };
}

export interface FromValidation {
    /** The request body, or null when a field is invalid. */
    policy: SmtpFromPolicy | null;
    domainError: string | null;
    addressError: string | null;
}

/** Splits an address at its last @; null when there is none. */
function splitAddress(address: string): { local: string; domain: string } | null {
    const at = address.lastIndexOf('@');
    if (at < 1 || at === address.length - 1) return null;
    return { local: address.slice(0, at), domain: address.slice(at + 1) };
}

const byteLength = (text: string) => new TextEncoder().encode(text).length;

/** The domain of the configured address, lowercased, when the address has one. */
export function addressDomain(address: string): string | null {
    return splitAddress(address.trim())?.domain.toLowerCase() ?? null;
}

/**
 * Mirrors the server's checks so mistakes show before saving. The server stays authoritative: it also
 * requires the domain to be a verified sending domain of the app.
 */
export function validateFromForm(form: FromForm, verifiedDomains: readonly string[]): FromValidation {
    const none: FromValidation = { policy: null, domainError: null, addressError: null };
    if (form.mode === 'off') return { ...none, policy: { mode: 'off', only_unauthorized: form.onlyUnauthorized } };
    if (form.mode === 'domain') {
        if (!form.domain) return { ...none, domainError: 'Choose a domain.' };
        if (!verifiedDomains.includes(form.domain)) return { ...none, domainError: 'That domain isn\'t a verified sending domain of this app.' };
        return { ...none, policy: { mode: 'domain', domain: form.domain, only_unauthorized: form.onlyUnauthorized } };
    }
    const address = form.address.trim();
    if (!address) return { ...none, addressError: 'Enter the address to send from.' };
    const parts = splitAddress(address);
    if (!parts || parts.local.includes('@')) return { ...none, addressError: 'Enter a full address, such as no-reply@example.com.' };
    if (/[^\x00-\x7f]/.test(address)) return { ...none, addressError: 'Use plain letters, numbers and symbols only; accented and non-Latin characters aren\'t supported.' };
    if (!DOT_ATOM.test(parts.local) || byteLength(parts.local) > MAX_LOCAL_PART_BYTES) {
        return { ...none, addressError: 'The part before the @ can use letters, numbers and these symbols: ! # $ % & \' * + / = ? ^ _ ` { | } ~ - . It can\'t start, end or double up dots, or run over 64 characters.' };
    }
    if (byteLength(address) > MAX_ADDRESS_BYTES) return { ...none, addressError: 'That address is too long.' };
    const domain = parts.domain.toLowerCase();
    if (!verifiedDomains.includes(domain)) return { ...none, addressError: `${domain} isn't a verified sending domain of this app. Verify it on Sending domains first.` };
    return { ...none, policy: { mode: 'address', address: `${parts.local}@${domain}`, only_unauthorized: form.onlyUnauthorized } };
}

export interface FromExample {
    sent: string;
    becomes: string;
}

const EXAMPLE_ORIGIN = { local: 'scanner', domain: 'office.example.net' };

/**
 * The example sender's encoded name part, shortened the way the server does when
 * the target domain leaves less room (64 octets for the name part, 254 for the
 * whole address). The real hash depends on the address, so it's shown as <hash>.
 */
function encodedExampleLocal(target: string): string | null {
    const local = `${EXAMPLE_ORIGIN.local}=${EXAMPLE_ORIGIN.domain}`;
    const limit = Math.min(64, MAX_ADDRESS_BYTES - 1 - byteLength(target));
    if (limit < 10) return null; // like the server: no room for a dash and an 8-character hash
    if (byteLength(local) <= limit) return local;
    const prefix = local.slice(0, Math.max(0, limit - 9)).replace(/\.+$/, '');
    return `${prefix}-<hash>`;
}

/** What the chosen policy does to a typical From address, for the form's examples. */
export function fromExamples(form: FromForm, verifiedDomains: readonly string[]): FromExample[] {
    const sent = `${EXAMPLE_ORIGIN.local}@${EXAMPLE_ORIGIN.domain}`;
    if (form.mode === 'off') return [];
    const examples: FromExample[] = [];
    if (form.mode === 'domain' && form.domain) {
        const target = form.domain;
        const local = encodedExampleLocal(target);
        examples.push({ sent, becomes: local === null ? 'Refused: this domain is too long to send from' : `${local}@${target}` });
    } else if (form.mode === 'address' && form.address.trim()) {
        examples.push({ sent, becomes: form.address.trim() });
    }
    if (form.onlyUnauthorized && verifiedDomains.length) {
        const own = `billing@${form.mode === 'domain' && form.domain ? form.domain : verifiedDomains[0]}`;
        examples.push({ sent: own, becomes: `${own} (unchanged)` });
    }
    return examples;
}

/** Domains the policy targets whose mail would not be DKIM-signed right now. */
export function unsignedDomains(signingReady: AppSmtp['settings']['signing_ready']): string[] {
    return (signingReady ?? []).filter(entry => !entry.ready).map(entry => entry.domain);
}

export function activeCredentialCount(credentials: readonly SmtpCredential[]): number {
    return credentials.filter(credential => credential.active).length;
}
