import type { App } from './openapi-client-generated';

const nameCollator = new Intl.Collator(undefined, { sensitivity: 'base', numeric: true });

/** Apps by name, ignoring case ("App 2" before "App 10"); equal names keep the API's order (oldest first). */
export function sortAppsByName<T extends Pick<App, 'name'>>(apps: readonly T[]): T[] {
    return [...apps].sort((a, b) => nameCollator.compare(a.name, b.name));
}
