import { computed, shallowRef } from 'vue';

import { DefaultApi, type RuntimeInfo } from './openapi-client-generated';
import { parseRuntimeInfo } from './tracking';

const runtime = shallowRef<RuntimeInfo | null>(null);
let loading: Promise<void> | null = null;

/**
 * Reads the server's enabled features once and shares the result. A failed read is not remembered: the next
 * call tries again, and until one succeeds the features that need the server's say-so stay hidden.
 */
export function loadRuntimeFeatures(): Promise<void> {
    if (runtime.value) return Promise.resolve();
    loading ??= (async () => {
        try {
            const { data } = await DefaultApi.getRuntime();
            runtime.value = parseRuntimeInfo(data);
        } catch {
            runtime.value = null;
        } finally {
            loading = null;
        }
    })();
    return loading;
}

/** True only when the server reported that it accepts mail over SMTP; false while unknown. */
export const smtpSubmissionEnabled = computed(() => runtime.value?.features.smtp_submission === true);

/** The loaded report, or null while unknown. */
export const runtimeFeatures = computed(() => runtime.value?.features ?? null);

/**
 * What the server reports about tracking: null while unknown or when tracking is off. `custom_domains` says whether
 * apps may use their own tracking domain, and `cname_target` is the host a DNS record should point at.
 */
export const runtimeTracking = computed(() => runtime.value?.tracking ?? null);

/** Forgets the shared report; for tests and for sign-out. */
export function resetRuntimeFeatures(): void {
    runtime.value = null;
    loading = null;
}
