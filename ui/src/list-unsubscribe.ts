import { InvalidResponseError } from './api-errors';
import type { ListUnsubscribe } from './openapi-client-generated';

/** Validates the opt-in response: a single boolean. */
export function parseListUnsubscribe(data: unknown): ListUnsubscribe {
    const candidate = data as Partial<ListUnsubscribe> | null | undefined;
    if (typeof candidate?.enabled !== 'boolean') throw new InvalidResponseError();
    return { enabled: candidate.enabled };
}
