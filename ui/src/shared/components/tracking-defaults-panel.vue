<template>
    <section class="card" aria-labelledby="tracking-defaults-heading" :aria-busy="isLoading || isSaving">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 id="tracking-defaults-heading">Tracking defaults</h2>
                <HelpButton topic="tracking defaults">
                    <p>
                        Whether this app's HTML messages get open and click tracking when a send doesn't say either way. Both are off by
                        default. A message's own setting always wins, whether it turns tracking on or off.
                    </p>
                    <ul>
                        <li>
                            A message sets its own tracking with Postmark <code>TrackOpens</code> and <code>TrackLinks</code>, SendGrid
                            <code>tracking_settings</code>, Mailgun <code>o:tracking-opens</code> and <code>o:tracking-clicks</code>, or the Resend headers
                            <code>X-Thinpost-Track-Opens</code> and <code>X-Thinpost-Track-Clicks</code>.
                        </li>
                        <li>A default never causes a message to be rejected. If a message is too large or has too many links to track, click tracking is dropped first, then open tracking. A message's own choices always stand.</li>
                        <li>A change only affects messages submitted afterwards. Messages already submitted keep their settings.</li>
                        <li>Defaults have no effect while tracking isn't available for this Thinpost installation.</li>
                        <li>Events are recorded only up to the daily tracking limit below. Links are still tracked when the limit is zero, but nothing is recorded.</li>
                    </ul>
                </HelpButton>
            </div>
            <button type="button" class="btn sm" :disabled="isLoading || isSaving || !isSignedIn" @click="load">
                <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isLoading && !!config }" aria-hidden="true" />Refresh
            </button>
        </div>
        <div class="card-body panel-body">
            <Loader v-if="isLoading && !config" label="Loading tracking defaults" />
            <ApiErrorNotice v-if="loadError" :error="loadError" :retry="isSignedIn ? load : undefined" retry-label="Reload" />
            <dl v-if="config" class="facts" data-testid="tracking-defaults-facts">
                <div v-for="row in rows" :key="row.kind">
                    <dt>{{ row.label }}</dt>
                    <dd>
                        <span v-if="config[row.kind]" class="tag success">On</span>
                        <span v-else class="tag">Off</span>
                        <span v-if="config[row.kind] && tracking === false" class="tag warning" :data-testid="`tracking-defaults-${row.kind}-inert`">No effect</span>
                        <button
                            type="button"
                            class="btn sm"
                            :class="{ primary: !config[row.kind] }"
                            :disabled="isLoading || isSaving || !isSignedIn || (!config[row.kind] && tracking === false)"
                            :data-testid="`tracking-defaults-${row.kind}-toggle`"
                            @click="toggle(row.kind)"
                        >
                            {{ config[row.kind] ? 'Turn off' : 'Turn on' }}
                        </button>
                    </dd>
                </div>
            </dl>
            <p v-if="config && tracking === false" class="hint" data-testid="tracking-defaults-blocker">Tracking isn't available for this Thinpost installation, so these defaults have no effect. Ask your Thinpost administrator to enable tracking.</p>
            <p v-if="config && !isSignedIn" class="hint">Sign in again to change tracking defaults.</p>
            <ApiErrorNotice v-if="saveError" :error="saveError" />
            <p v-if="savedNote" class="saved" role="status"><i class="fa-solid fa-check" aria-hidden="true" /> {{ savedNote }}</p>
        </div>
    </section>
</template>

<script setup lang="ts">
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, shallowRef } from 'vue';

import { apiErrorStatus, describeApiError, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { DefaultApi, type TrackingDefaults } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import { parseTrackingDefaults } from '@/tracking';

type Kind = keyof TrackingDefaults;

defineProps<{
    /** Whether tracking is available; null when that is not known (the server then decides). */
    tracking: boolean | null;
}>();

const rows: { kind: Kind; label: string }[] = [
    { kind: 'opens', label: 'Opens' },
    { kind: 'clicks', label: 'Clicks' }
];

const { app } = useAppContext();

const config = shallowRef<TrackingDefaults | null>(null);
const isLoading = ref(false);
const isSaving = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const saveError = ref<ApiErrorDescription | null>(null);
const savedNote = ref<string | null>(null);

const isSignedIn = computed(() => authState.status === 'signed-in');

let seq = 0;
let isUnmounted = false;

async function load() {
    const mine = ++seq;
    isLoading.value = true;
    loadError.value = null;
    savedNote.value = null;
    try {
        const { data } = await DefaultApi.getTrackingDefaults({ path: { appId: app.value.id } });
        if (isUnmounted || mine !== seq) return;
        config.value = parseTrackingDefaults(data);
    } catch (err) {
        if (isUnmounted || mine !== seq) return;
        // Without a loaded value there is nothing safe to toggle.
        config.value = null;
        loadError.value = describeApiError(err, {
            notFound: 'The app was not found.',
            unavailable: 'Tracking defaults are temporarily unavailable. Try again in a moment.'
        });
    } finally {
        if (mine === seq) isLoading.value = false;
    }
}

async function toggle(kind: Kind) {
    const current = config.value;
    if (!current || isSaving.value) return;
    const target = app.value;
    const enable = !current[kind];
    if (enable) {
        const ok = await showConfirm(
            kind === 'clicks' ? 'Turn on click tracking by default?' : 'Turn on open tracking by default?',
            kind === 'clicks'
                ? `Links in HTML messages from “${target.name}” that don't set click tracking will be tracked. Turn it off per message for password-reset and sign-in links.`
                : `HTML messages from “${target.name}” that don't set open tracking will include a tiny tracking image.`
        );
        if (!ok || isUnmounted) return;
    }
    isSaving.value = true;
    saveError.value = null;
    savedNote.value = null;
    seq++;
    isLoading.value = false;
    // Both values are sent: the toggled kind and the other kind as loaded.
    const body: TrackingDefaults = { ...current, [kind]: enable };
    try {
        const { data } = await DefaultApi.setTrackingDefaults({ path: { appId: target.id }, body });
        const saved = parseTrackingDefaults(data);
        if (isUnmounted) {
            showToast({ message: `Tracking defaults for “${target.name}” were saved after you left the page.`, durationSecs: 8 });
            return;
        }
        config.value = saved;
        savedNote.value = `Saved: opens ${saved.opens ? 'on' : 'off'}, clicks ${saved.clicks ? 'on' : 'off'}.`;
        showToast({ message: `Tracking defaults saved for “${target.name}”.`, durationSecs: 5 });
    } catch (err) {
        if (isUnmounted) {
            showToast({ message: `The tracking defaults change for “${target.name}” may not have completed. Open its Tracking tab to check it.`, durationSecs: 8 });
            return;
        }
        const status = apiErrorStatus(err);
        saveError.value = describeApiError(err, {
            mutation: true,
            notFound: 'The app was not found. Nothing was changed.',
            conflict: 'Tracking isn\'t available for this Thinpost installation, so a default can\'t be turned on. Ask your Thinpost administrator to enable tracking. Nothing was changed.',
            unavailable: 'Changing tracking defaults is temporarily unavailable. Try again in a moment.'
        });
        // Without a definitive refusal the stored value may differ from the one shown.
        if (status === undefined || status >= 500) void load();
    } finally {
        isSaving.value = false;
    }
}

void load();

onBeforeUnmount(() => {
    isUnmounted = true;
});
</script>

<style lang="scss" scoped>
.card-header {
    flex-wrap: wrap;
}

.panel-body {
    display: flex;
    flex-direction: column;
    gap: 10px;
    font-size: 13px;

    dd {
        display: flex;
        align-items: center;
        gap: 8px;
        white-space: nowrap;
    }
}

.saved {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--success);
}
</style>
