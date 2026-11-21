<template>
    <div id="app-tracking">
        <section class="card" aria-labelledby="tracking-deployment-heading" :aria-busy="runtimeLoading">
            <div class="card-header">
                <div class="heading-with-help">
                    <h2 id="tracking-deployment-heading">
                        Tracking availability
                        <template v-if="runtime">
                            <span v-if="runtime.features.tracking" class="tag success">Available</span>
                            <span v-else class="tag warning">Not available</span>
                        </template>
                    </h2>
                    <HelpButton topic="tracking availability">
                        <p>
                            <strong>Available:</strong> open and click tracking is turned on for this Thinpost installation. Your app defaults and
                            per-message settings apply.
                        </p>
                        <p>
                            <strong>Not available:</strong> tracking isn't turned on for this Thinpost installation, so your Thinpost administrator needs to
                            enable it. Until then, sends that ask for tracking are rejected, app defaults have no effect, and links go out unchanged.
                        </p>
                    </HelpButton>
                </div>
                <button type="button" class="btn sm" :disabled="runtimeLoading" @click="loadRuntime">
                    <i class="fa-solid fa-rotate" :class="{ 'fa-spin': runtimeLoading && !!runtime }" aria-hidden="true" />Refresh
                </button>
            </div>
            <div class="card-body tracking-body">
                <Loader v-if="runtimeLoading && !runtime" label="Loading tracking availability" />
                <ApiErrorNotice v-if="runtimeError" :error="runtimeError" :retry="loadRuntime" />
                <template v-if="runtime">
                    <dl v-if="runtime.features.tracking && runtime.tracking" class="facts" data-testid="tracking-facts">
                        <div>
                            <dt>Tracking address</dt>
                            <dd class="mono">{{ runtime.tracking.origin }}</dd>
                        </div>
                        <div>
                            <dt>Events per link</dt>
                            <dd>{{ formatCount(runtime.tracking.link_event_limit) }}</dd>
                        </div>
                    </dl>
                    <p v-else class="muted">Tracking isn't turned on for this Thinpost installation. Ask your Thinpost administrator to enable it. Until then, sends that ask for tracking are rejected, app defaults have no effect, and links go out unchanged.</p>
                </template>
            </div>
        </section>

        <TrackingDomainsSummary v-if="runtime && runtime.features.tracking && runtime.tracking" :app-id="app.id" :available="runtime.tracking.custom_domains" />

        <TrackingDefaultsPanel :tracking="runtime ? runtime.features.tracking : null" />

        <section class="card" aria-labelledby="tracking-budget-heading" :aria-busy="quotaLoading || isSubmitting">
            <div class="card-header">
                <div class="heading-with-help">
                    <h2 id="tracking-budget-heading">Daily tracking limit (UTC)</h2>
                    <HelpButton topic="daily tracking limit">
                        <p>The most opens and clicks Thinpost records per UTC day for this app. The default is zero, so nothing is recorded until you set a limit.</p>
                        <ul>
                            <li>Tracked links always work and redirect, whatever the limit.</li>
                            <li>“Recorded today” counts the events kept. “Over limit” counts visits that weren't recorded because a limit was reached.</li>
                            <li>Email scanners and privacy proxies load links and images too, so counts include automated visits. They are not counts of people.</li>
                        </ul>
                    </HelpButton>
                </div>
                <div class="actions">
                    <button type="button" class="btn sm" :disabled="quotaLoading || isSubmitting || !isSignedIn" @click="reloadQuota">
                        <i class="fa-solid fa-rotate" :class="{ 'fa-spin': quotaLoading && !!quota }" aria-hidden="true" />Refresh
                    </button>
                    <button type="button" class="btn sm primary" :disabled="!quota || !!blocker || quotaLoading || isSubmitting" @click="open">
                        <i class="fa-solid fa-pen" aria-hidden="true" />Edit
                    </button>
                </div>
            </div>
            <div class="card-body tracking-body">
                <Loader v-if="quotaLoading && !quota" label="Loading daily tracking limit" />
                <ApiErrorNotice v-if="quotaError" :error="quotaError" :retry="isSignedIn ? reloadQuota : undefined" retry-label="Reload" />
                <template v-if="quota">
                    <dl class="facts" data-testid="quota-facts">
                        <div>
                            <dt>Limit</dt>
                            <dd>
                                {{ formatCount(quota.daily_limit) }}
                                <span v-if="quota.daily_limit === 0" class="tag warning">Recording off</span>
                            </dd>
                        </div>
                        <div>
                            <dt>Recorded today</dt>
                            <dd>{{ formatCount(quota.used) }}</dd>
                        </div>
                        <div>
                            <dt>Over limit</dt>
                            <dd>{{ formatCount(quota.suppressed) }}</dd>
                        </div>
                        <div>
                            <dt>UTC day</dt>
                            <dd>{{ quota.day }}</dd>
                        </div>
                    </dl>
                    <p class="hint">As of <time :datetime="loadedAt.toISOString()">{{ formatTime(loadedAt) }}</time></p>
                </template>
                <p v-if="blocker && quota" class="hint">{{ blocker }}</p>
                <div v-if="isStale && !isModalOpen" class="notice warning" role="status">
                    <i class="fa-solid fa-circle-info" aria-hidden="true" />
                    <p class="notice-body">An earlier change may or may not have gone through, so the limit shown may be out of date. Refresh to check it.</p>
                </div>
                <p v-if="savedNote" class="saved" role="status"><i class="fa-solid fa-check" aria-hidden="true" /> {{ savedNote }}</p>
            </div>

            <TrackingQuotaModal
                v-if="isModalOpen && quota"
                :app-id="app.id"
                :app-name="app.name"
                :current="quota"
                :loaded-at="loadedAt"
                :is-submitting="isSubmitting"
                :error="submitError"
                :blocker="blocker"
                @close="close"
                @submit="save"
            />
        </section>

        <ListUnsubscribePanel :tracking="runtime ? runtime.features.tracking : null" />
    </div>
</template>

<script setup lang="ts">
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, shallowRef } from 'vue';
import { onBeforeRouteLeave, onBeforeRouteUpdate } from 'vue-router';

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState, registerLeaveGuard } from '@/auth';
import { DefaultApi, type RuntimeInfo, type TrackingQuota } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import ListUnsubscribePanel from '@/shared/components/list-unsubscribe-panel.vue';
import Loader from '@/shared/components/loader.vue';
import TrackingDefaultsPanel from '@/shared/components/tracking-defaults-panel.vue';
import TrackingDomainsSummary from '@/shared/components/tracking-domains-summary.vue';
import TrackingQuotaModal from '@/shared/components/tracking-quota-modal.vue';
import { formatCount } from '@/submission-rate';
import { parseRuntimeInfo, parseTrackingQuota } from '@/tracking';

const { app } = useAppContext();

/** Statuses after which the server definitely did not apply the change. */
const DEFINITIVE_REFUSALS = new Set([400, 401, 403, 404]);
const timeFormat = new Intl.DateTimeFormat(undefined, { timeStyle: 'medium' });

const runtime = shallowRef<RuntimeInfo | null>(null);
const runtimeLoading = ref(false);
const runtimeError = ref<ApiErrorDescription | null>(null);

const quota = shallowRef<TrackingQuota | null>(null);
const loadedAt = ref(new Date());
const quotaLoading = ref(false);
const quotaError = ref<ApiErrorDescription | null>(null);
const isModalOpen = ref(false);
const isSubmitting = ref(false);
const submitError = ref<ApiErrorDescription | null>(null);
const savedNote = ref<string | null>(null);
const isStale = ref(false);

const isSignedIn = computed(() => authState.status === 'signed-in');
const blocker = computed(() => (isSignedIn.value ? null : 'Sign in again to change the daily tracking limit.'));

let runtimeSeq = 0;
let quotaSeq = 0;
let isUnmounted = false;

function formatTime(date: Date) {
    return timeFormat.format(date);
}

async function loadRuntime() {
    const seq = ++runtimeSeq;
    runtimeLoading.value = true;
    runtimeError.value = null;
    try {
        const { data } = await DefaultApi.getRuntime();
        if (isUnmounted || seq !== runtimeSeq) return;
        runtime.value = parseRuntimeInfo(data);
    } catch (err) {
        if (isUnmounted || seq !== runtimeSeq) return;
        // Never show a stale availability state as current.
        runtime.value = null;
        runtimeError.value = describeApiError(err, { unavailable: 'Tracking availability couldn\'t be loaded right now. Try again in a moment.' });
    } finally {
        if (seq === runtimeSeq) runtimeLoading.value = false;
    }
}

async function loadQuota() {
    const seq = ++quotaSeq;
    quotaLoading.value = true;
    quotaError.value = null;
    try {
        const { data } = await DefaultApi.getTrackingQuota({ path: { appId: app.value.id } });
        if (isUnmounted || seq !== quotaSeq) return;
        quota.value = parseTrackingQuota(data);
        loadedAt.value = new Date();
        isStale.value = false;
    } catch (err) {
        if (isUnmounted || seq !== quotaSeq) return;
        const description = describeApiError(err, {
            notFound: 'The app was not found.',
            unavailable: 'The daily tracking limit is temporarily unavailable. Try again in a moment.'
        });
        quotaError.value = quota.value ? { ...description, message: `${description.message} The limit shown is from the last time it loaded.` } : description;
    } finally {
        if (seq === quotaSeq) quotaLoading.value = false;
    }
}

function reloadQuota() {
    savedNote.value = null;
    void loadQuota();
}

function open() {
    if (!quota.value || blocker.value || quotaLoading.value || isSubmitting.value) return;
    submitError.value = null;
    savedNote.value = null;
    isModalOpen.value = true;
}

function close() {
    if (isSubmitting.value) return;
    isModalOpen.value = false;
}

async function save(dailyLimit: number) {
    if (isSubmitting.value || blocker.value) return;
    const target = app.value;
    isSubmitting.value = true;
    submitError.value = null;
    savedNote.value = null;
    // A newer reload must not overwrite the saved result with an older read.
    quotaSeq++;
    quotaLoading.value = false;

    let saved: TrackingQuota | null = null;
    let outcome: 'succeeded' | 'refused' | 'unknown';
    try {
        const { data } = await DefaultApi.setTrackingQuota({ path: { appId: target.id }, body: { daily_limit: dailyLimit } });
        saved = parseTrackingQuota(data);
        outcome = 'succeeded';
    } catch (err) {
        const status = apiErrorStatus(err);
        const description = describeApiError(err, {
            mutation: true,
            notFound: 'The app was not found. Nothing was changed.',
            unavailable: 'Changing the limit is temporarily unavailable. Try again in a moment.'
        });
        if (status !== undefined && DEFINITIVE_REFUSALS.has(status) && !(err instanceof InvalidResponseError)) {
            outcome = 'refused';
            submitError.value = status === 404 ? description : { ...description, message: `${description.message} Nothing was changed.` };
        } else {
            outcome = 'unknown';
            isStale.value = true;
            submitError.value = { ...description, message: `${description.message} It is safe to save the same value again.` };
        }
    } finally {
        isSubmitting.value = false;
    }

    if (isUnmounted) {
        const message =
            outcome === 'succeeded'
                ? `The daily tracking limit for “${target.name}” was saved after you left the page: ${formatCount(saved!.daily_limit)} per day.`
                : outcome === 'unknown'
                  ? `The daily tracking limit change for “${target.name}” may or may not have gone through. Open its Tracking tab to check it.`
                  : `The daily tracking limit change for “${target.name}” was refused. Nothing was changed.`;
        showToast({ message, durationSecs: 10 });
        return;
    }
    if (outcome !== 'succeeded' || !saved) return;
    quota.value = saved;
    loadedAt.value = new Date();
    quotaError.value = null;
    isStale.value = false;
    isModalOpen.value = false;
    savedNote.value = `Saved: ${formatCount(saved.daily_limit)} events per day.`;
    showToast({ message: `Daily tracking limit for “${target.name}” set to ${formatCount(saved.daily_limit)}.`, durationSecs: 5 });
}

async function confirmLeave() {
    if (!isSubmitting.value) return true;
    return showConfirm(
        'Leave while the limit is being saved?',
        'The request has been sent and may still go through. This page won\'t show the result. Open the Tracking tab afterwards to check the limit.'
    );
}

onBeforeRouteLeave(confirmLeave);
onBeforeRouteUpdate(confirmLeave);
// Sign-in and sign-out leave the page without a route change.
const unregisterLeaveGuard = registerLeaveGuard(confirmLeave);

void loadRuntime();
void loadQuota();

onBeforeUnmount(() => {
    isUnmounted = true;
    unregisterLeaveGuard();
});
</script>

<style lang="scss" scoped>
#app-tracking {
    display: flex;
    flex-direction: column;
    gap: 16px;
}

.card-header h2 {
    display: flex;
    align-items: center;
    gap: 8px;
}

.actions {
    display: flex;
    gap: 8px;
}

.tracking-body {
    display: flex;
    flex-direction: column;
    gap: 10px;
    font-size: 13px;

    dd {
        white-space: nowrap;
    }
}

.mono {
    font-family: var(--mono, monospace);
    overflow: hidden;
    text-overflow: ellipsis;
}

.saved {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--success);
}
</style>
