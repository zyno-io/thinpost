<template>
    <section :aria-labelledby="`${id}-heading`" class="count-limit card" :data-limit="kind" :aria-busy="isLoading || isSubmitting">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 :id="`${id}-heading`">
                    {{ config.title }}
                    <template v-if="current">
                        <span v-if="isBlocked(current)" class="tag warning">New messages blocked</span>
                        <span v-else class="tag success">Limited</span>
                    </template>
                </h2>
                <HelpButton :topic="config.noun">
                    <template v-if="kind === 'recipient'">
                        <p>
                            This limits how many recipients this app can send to each UTC day. Messages over the limit are rejected with a “too many
                            requests” error, so your code should retry after the day rolls over or the limit is raised.
                        </p>
                        <ul>
                            <li>Every To, Cc and Bcc address counts, including suppressed recipients. Recipients are counted when a message is accepted.</li>
                            <li>Changing the limit does not reset today's count. Raising it takes effect for the rest of the day.</li>
                            <li>A limit of zero rejects every new message. New apps start at zero.</li>
                            <li>A paused app rejects all new messages, whatever its limit.</li>
                        </ul>
                    </template>
                    <template v-else>
                        <p>
                            This limits how many of this app's messages can be waiting to be delivered at once. Messages over the limit are rejected
                            with a “too many requests” error, so your code should retry later.
                        </p>
                        <ul>
                            <li>A message counts as pending from when it is accepted until it finishes delivery. A held message counts until it is cancelled or expires.</li>
                            <li>The pending count is a snapshot and approximate. It is a lower bound when the panel says “or more”.</li>
                            <li>A limit of zero rejects every new message. New apps start at zero.</li>
                            <li>A paused app rejects all new messages, whatever its limit.</li>
                        </ul>
                    </template>
                </HelpButton>
            </div>
            <div class="limit-actions">
                <button type="button" class="btn sm" :disabled="isLoading || isSubmitting || !isSignedIn" @click="reload">
                    <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isLoading && !!current }" aria-hidden="true" />
                    {{ isLoading && current ? 'Reloading…' : 'Reload limit' }}
                </button>
                <button
                    class="btn primary"
                    type="button"
                    :disabled="!current || !!blocker || isLoading || isSubmitting"
                    :aria-describedby="current && blocker ? `${id}-blocker` : undefined"
                    @click="open"
                >
                    <i class="fa-solid fa-gauge-high" aria-hidden="true" />
                    Change limit
                </button>
            </div>
        </div>

        <div class="card-body limit-body">
            <p v-if="current" class="hint">As of <time :datetime="loadedAt.toISOString()">{{ formatTime(loadedAt) }}</time></p>

            <Loader v-if="isLoading && !current" :label="`Loading ${config.noun}`" />

            <ApiErrorNotice v-if="loadError" :error="loadError" :retry="isSignedIn ? reload : undefined" retry-label="Reload limit" />

            <template v-if="current">
                <div v-if="isBlocked(current)" class="notice warning">
                    <i class="fa-solid fa-ban" aria-hidden="true" />
                    <p class="notice-body">
                        <strong>New messages are blocked.</strong> Every message is rejected until a limit is set. New apps start blocked.
                    </p>
                </div>

                <dl v-else class="facts">
                    <div>
                        <dt>Limit</dt>
                        <dd>{{ formatCount(current.limit) }} {{ config.unit }}</dd>
                    </div>
                    <div>
                        <dt>{{ config.usageLabel }}</dt>
                        <dd>{{ describeUsage(config, current) }}</dd>
                    </div>
                    <div v-if="current.day">
                        <dt>Day (UTC)</dt>
                        <dd>{{ current.day }}</dd>
                    </div>
                </dl>

                <p v-if="isBlocked(current)" class="hint">
                    {{ config.usageLabel }}: {{ formatCount(current.usage) }}{{ current.truncated ? ' or more' : '' }}.
                </p>
                <p v-if="current.truncated" class="hint">The pending count is approximate: more messages are pending than Thinpost counts.</p>
            </template>

            <p v-if="current && blocker" :id="`${id}-blocker`" class="blocker">{{ blocker }}</p>

            <div v-if="staleNote" class="notice warning" role="status">
                <i class="fa-solid fa-circle-info" aria-hidden="true" />
                <p class="notice-body">{{ staleNote }}</p>
            </div>

            <p v-if="savedNote" class="saved" role="status">
                <i class="fa-solid fa-check" aria-hidden="true" />
                {{ savedNote }}
            </p>
        </div>

        <CountLimitModal
            v-if="isModalOpen && current"
            :config="config"
            :app-id="app.id"
            :app-name="app.name"
            :current="current"
            :loaded-at="loadedAt"
            :is-submitting="isSubmitting"
            :error="submitError"
            :blocker="blocker"
            @close="close"
            @submit="save"
        />
    </section>
</template>

<script setup lang="ts">
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, shallowRef, useId } from 'vue';
import { onBeforeRouteLeave, onBeforeRouteUpdate } from 'vue-router';

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState, registerLeaveGuard } from '@/auth';
import { COUNT_LIMITS, type CountLimit, type CountLimitKind, describeUsage, isBlocked, summarizeLimit } from '@/count-limits';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import CountLimitModal from '@/shared/components/count-limit-modal.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import { formatCount } from '@/submission-rate';

const props = defineProps<{ kind: CountLimitKind }>();
const config = COUNT_LIMITS[props.kind];

const { app } = useAppContext();

/** Statuses after which the server definitely did not apply the change. */
const DEFINITIVE_REFUSALS = new Set([400, 401, 403, 404]);

const id = useId();
const timeFormat = new Intl.DateTimeFormat(undefined, { timeStyle: 'medium' });

const current = shallowRef<CountLimit | null>(null);
const loadedAt = ref(new Date());
const isLoading = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const isModalOpen = ref(false);
const isSubmitting = ref(false);
const submitError = ref<ApiErrorDescription | null>(null);
const savedNote = ref<string | null>(null);
/** An earlier change has an unknown outcome; the setting shown may be out of date. */
const isStale = ref(false);

const isSignedIn = computed(() => authState.status === 'signed-in');
const blocker = computed(() => (isSignedIn.value ? null : `Sign in again to change the ${config.noun}.`));
const staleNote = computed(() =>
    isStale.value && !isModalOpen.value
        ? 'An earlier change may or may not have gone through, so the setting shown may be out of date. Reload the limit to check it.'
        : null
);

let loadSeq = 0;
let isUnmounted = false;

function formatTime(date: Date) {
    return timeFormat.format(date);
}

async function load() {
    if (isUnmounted) return;
    const seq = ++loadSeq;
    isLoading.value = true;
    loadError.value = null;
    try {
        const data = await config.load(app.value.id);
        if (isUnmounted || seq !== loadSeq) return;
        current.value = config.parse(data);
        loadedAt.value = new Date();
        isStale.value = false;
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        const description = describeApiError(err, {
            notFound: 'The app was not found.',
            unavailable: `The ${config.noun} is temporarily unavailable. Try again in a moment.`
        });
        loadError.value = current.value
            ? { ...description, message: `${description.message} The setting shown is from the last time it loaded.` }
            : description;
    } finally {
        if (seq === loadSeq) isLoading.value = false;
    }
}

function reload() {
    savedNote.value = null;
    void load();
}

function open() {
    if (!current.value || blocker.value || isLoading.value || isSubmitting.value) return;
    submitError.value = null;
    savedNote.value = null;
    isModalOpen.value = true;
}

function close() {
    if (isSubmitting.value) return;
    isModalOpen.value = false;
}

async function save(requested: number) {
    if (isSubmitting.value || blocker.value) return;
    const target = app.value;
    isSubmitting.value = true;
    submitError.value = null;
    savedNote.value = null;
    // A newer reload must not overwrite the saved result with an older read.
    loadSeq++;
    isLoading.value = false;

    let saved: CountLimit | null = null;
    let outcome: 'succeeded' | 'refused' | 'unknown';
    try {
        saved = config.parse(await config.save(target.id, requested));
        outcome = 'succeeded';
    } catch (err) {
        const status = apiErrorStatus(err);
        const description = describeApiError(err, {
            mutation: true,
            notFound: 'The app was not found. Nothing was changed.',
            unavailable: `Changing the ${config.noun} is temporarily unavailable. Try again in a moment.`
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
                ? `The ${config.noun} for “${target.name}” was saved after you left the page: ${summarizeLimit(config, saved!)}.`
                : outcome === 'unknown'
                  ? `The ${config.noun} change for “${target.name}” may or may not have gone through. Reload the app to check it.`
                  : `The ${config.noun} change for “${target.name}” was refused. Nothing was changed.`;
        showToast({ message, durationSecs: 10 });
        return;
    }

    if (outcome !== 'succeeded' || !saved) return;
    current.value = saved;
    loadedAt.value = new Date();
    loadError.value = null;
    isStale.value = false;
    isModalOpen.value = false;
    const capitalized = config.noun.charAt(0).toUpperCase() + config.noun.slice(1);
    const message = isBlocked(saved)
        ? `New messages blocked for “${target.name}”.`
        : `${capitalized} for “${target.name}” set to ${summarizeLimit(config, saved)}.`;
    savedNote.value = `Saved: ${summarizeLimit(config, saved)}.`;
    showToast({ message, durationSecs: 5 });
}

async function confirmLeave() {
    if (!isSubmitting.value) return true;
    return showConfirm(
        `Leave while the ${config.noun} is being saved?`,
        "The request has been sent and may still go through. This page won't show the result. Reload the app afterwards to check the limit."
    );
}

onBeforeRouteLeave(confirmLeave);
onBeforeRouteUpdate(confirmLeave);
// Sign-in and sign-out leave the page without a route change.
const unregisterLeaveGuard = registerLeaveGuard(confirmLeave);

void load();

onBeforeUnmount(() => {
    isUnmounted = true;
    unregisterLeaveGuard();
});
</script>

<style lang="scss" scoped>
.card-header {
    flex-wrap: wrap;
}

.card-header h2 {
    display: flex;
    align-items: center;
    gap: 8px;
}

.limit-actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
}

.limit-body {
    display: flex;
    flex-direction: column;
    gap: 12px;
    font-size: 13px;
}

.blocker {
    color: var(--text-2);
}

.saved {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--success);
}
</style>
