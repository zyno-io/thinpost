const dateTimeFormat = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' });

export function formatDateTime(value: string): string {
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? value : dateTimeFormat.format(date);
}

const relativeFormat = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto', style: 'short' });
const RELATIVE_UNITS: Array<[Intl.RelativeTimeFormatUnit, number]> = [
    ['day', 86_400_000],
    ['hour', 3_600_000],
    ['minute', 60_000]
];

/** Compact time relative to now ("in 35 min"); past or imminent times read "now". The absolute time belongs in a title. */
export function formatRelativeTime(value: string, now = Date.now()): string {
    const time = new Date(value).getTime();
    if (Number.isNaN(time)) return value;
    const diff = time - now;
    if (diff < 60_000) return 'now';
    for (const [unit, ms] of RELATIVE_UNITS) {
        if (diff >= ms) return relativeFormat.format(Math.round(diff / ms), unit);
    }
    return 'now';
}

/** Copies text with an explicit result; clipboard access can be denied or unavailable. */
export async function copyText(value: string): Promise<boolean> {
    try {
        await navigator.clipboard.writeText(value);
        return true;
    } catch {
        return false;
    }
}

/** Leading characters of an opaque ID for compact display; the full ID stays in titles and labels. */
export function shortId(id: string, length = 12): string {
    return id.length > length ? id.slice(0, length) : id;
}

/** Human-readable byte count: `0 B`, then KiB/MiB/GiB with one decimal. */
export function formatBytes(bytes: number): string {
    if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
    if (bytes < 1024) return `${Math.floor(bytes)} B`;
    const units = ['KiB', 'MiB', 'GiB'];
    let value = bytes / 1024;
    let unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
        value /= 1024;
        unit++;
    }
    return `${value.toFixed(1)} ${units[unit]}`;
}
