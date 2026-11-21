import { OpenApiError } from '@zyno-io/openapi-client-codegen';

export type ApiErrorKind = 'unreachable' | 'invalid' | 'unauthenticated' | 'forbidden' | 'not-found' | 'conflict' | 'unavailable' | 'failed';

export interface ApiErrorDescription {
    kind: ApiErrorKind;
    status?: number;
    message: string;
}

export interface DescribeOptions {
    /** Message for HTTP 404. */
    notFound?: string;
    /** Message for HTTP 409. */
    conflict?: string;
    /** Message for HTTP 503, when the operation documents a specific cause. */
    unavailable?: string;
    /** The call changes state; a failure without a definitive response leaves the outcome unknown. */
    mutation?: boolean;
}

/** Thrown when a 2xx response body does not match the contract. */
export class InvalidResponseError extends Error {
    constructor() {
        super('Thinpost sent back something the console didn\'t understand.');
        this.name = 'InvalidResponseError';
    }
}

/** Returns the stable `error` code (for example `app_paused`) from a failed call's JSON body, if it has one. */
export function apiErrorCode(err: unknown): string | undefined {
    if (!(err instanceof OpenApiError)) return undefined;
    const body = err.body as { error?: unknown } | null | undefined;
    const code = body?.error;
    return typeof code === 'string' && /^[a-z_]+$/.test(code) ? code : undefined;
}

/** Returns the `reason` from a failed call's JSON body (for example `different_key` for a DKIM record check), if it has one. */
export function apiErrorReason(err: unknown): string | undefined {
    if (!(err instanceof OpenApiError)) return undefined;
    const reason = (err.body as { reason?: unknown } | null | undefined)?.reason;
    return typeof reason === 'string' && /^[a-z_]+$/.test(reason) ? reason : undefined;
}

/** Returns the HTTP status of a failed API call, if the server responded. */
export function apiErrorStatus(err: unknown): number | undefined {
    return err instanceof OpenApiError ? err.response?.status : undefined;
}

/**
 * Maps a failed API call to an user-facing description. The API
 * deliberately returns only the HTTP status text, so messages are derived from
 * the status alone and never echo request details.
 */
export const UNKNOWN_OUTCOME = ' We can\'t tell whether the change went through. Reload to check before trying again.';

/** For a 2xx response whose body does not match the contract (for example an HTML fallback page). */
export function invalidResponse(options: Pick<DescribeOptions, 'mutation'> = {}): ApiErrorDescription {
    return { kind: 'failed', message: `Thinpost sent back something the console didn't understand.${options.mutation ? UNKNOWN_OUTCOME : ''}` };
}

export function describeApiError(err: unknown, options: DescribeOptions = {}): ApiErrorDescription {
    if (err instanceof InvalidResponseError) return invalidResponse(options);
    const status = apiErrorStatus(err);
    const unknownOutcome = options.mutation ? UNKNOWN_OUTCOME : '';
    switch (status) {
        case undefined:
            return { kind: 'unreachable', message: `Couldn't reach Thinpost. Check your connection and try again.${unknownOutcome}` };
        case 400:
            return { kind: 'invalid', status, message: 'Thinpost rejected the request. Check what you entered and try again.' };
        case 401:
            return { kind: 'unauthenticated', status, message: 'Your session has ended. Sign in again.' };
        case 403:
            return { kind: 'forbidden', status, message: 'You are signed in, but your account isn\'t allowed to use this console. Ask your Thinpost administrator for access.' };
        case 404:
            return { kind: 'not-found', status, message: options.notFound ?? 'That item wasn\'t found. It may have been removed.' };
        case 409:
            if (options.conflict) return { kind: 'conflict', status, message: options.conflict };
            return { kind: 'conflict', status, message: 'That conflicts with the current state. Reload and try again.' };
        case 503:
            if (options.unavailable) return { kind: 'unavailable', status, message: `${options.unavailable}${unknownOutcome}` };
            return { kind: 'unavailable', status, message: `Thinpost is temporarily unavailable. Try again in a moment.${unknownOutcome}` };
        case 502:
        case 504:
            return { kind: 'unavailable', status, message: `Thinpost is temporarily unavailable. Try again in a moment.${unknownOutcome}` };
        default:
            return { kind: 'failed', status, message: `Something went wrong. Try again in a moment.${unknownOutcome}` };
    }
}
