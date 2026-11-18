<template>
    <section :aria-labelledby="`${id}-heading`" class="submission-rate card" :aria-busy="isLoading || isSubmitting">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 :id="`${id}-heading`">
                    Sending limit
                    <template v-if="rate">
                        <span v-if="isDisabled(rate)" class="tag warning">New messages blocked</span>
                        <span v-else class="tag success">Limited</span>
                    </template>
                </h2>
                <HelpButton topic="sending limit">
                    <p>
                        This limits how fast this app's API keys can send messages to Thinpost. Messages over the limit are rejected with a
                        “too many requests” error, so your code should retry later.
                    </p>
                    <ul>
                        <li>
                            The limit works like a bucket of tokens that refills steadily. Each message uses one token, however many recipients it has.
                            The burst is the size of the bucket.
                        </li>
                        <li>
                            A Resend batch needs tokens for all of its messages at once, and is rejected whole if there aren't enough. A batch larger
                            than the burst is never accepted. Postmark batches are counted message by message.
                        </li>
                        <li>
                            Tokens are used once a request passes validation. Retries, including retries with the same idempotency key, and requests
                            that fail afterwards still use tokens.
                        </li>
                        <li>A paused app rejects all new messages, whatever its limit.</li>
                    </ul>
                </HelpButton>
            </div>
            <div class="rate-actions">
                <button type="button" class="btn sm" :disabled="isLoading || isSubmitting || !isSignedIn" @click="reload">
                    <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isLoading && !!rate }" aria-hidden="true" />
                    {{ isLoading && rate ? 'Reloading…' : 'Reload limit' }}
                </button>
                <button class="btn primary"
                    type="button"
                    :disabled="!rate || !!blocker || isLoading || isSubmitting"
                    :aria-describedby="rate && blocker ? `${id}-blocker` : undefined"
                    @click="open"
                >
                    <i class="fa-solid fa-gauge-high" aria-hidden="true" />
                    Change limit
                </button>
            </div>
        </div>

        <div class="card-body rate-body">
            <p v-if="rate" class="hint">As of <time :datetime="loadedAt.toISOString()">{{ formatTime(loadedAt) }}</time></p>

            <Loader v-if="isLoading && !rate" label="Loading sending limit" />

            <ApiErrorNotice v-if="loadError" :error="loadError" :retry="isSignedIn ? reload : undefined" retry-label="Reload limit" />

            <template v-if="rate">
                <div v-if="isDisabled(rate)" class="notice warning">
                    <i class="fa-solid fa-ban" aria-hidden="true" />
                    <p class="notice-body">
                        <strong>New messages are blocked.</strong> Every message is rejected until a limit is set. New apps start blocked.
                    </p>
                </div>

                <dl v-else class="facts">
                    <div>
                        <dt>Rate</dt>
                        <dd>{{ describeSustained(rate.per_minute) }}</dd>
                    </div>
                    <div>
                        <dt>Burst</dt>
                        <dd>{{ formatCount(rate.burst) }} message{{ rate.burst === 1 ? '' : 's' }}</dd>
                    </div>
                    <div>
                        <dt>Time to refill</dt>
                        <dd>{{ describeRefill(rate) }}</dd>
                    </div>
                </dl>

            </template>

            <p v-if="rate && blocker" :id="`${id}-blocker`" class="blocker">{{ blocker }}</p>

            <div v-if="staleNote" class="notice warning" role="status">
                <i class="fa-solid fa-circle-info" aria-hidden="true" />
                <p class="notice-body">{{ staleNote }}</p>
            </div>

            <p v-if="savedNote" class="saved" role="status">
                <i class="fa-solid fa-check" aria-hidden="true" />
                {{ savedNote }}
            </p>
        </div>

        <SubmissionRateModal
            v-if="isModalOpen && rate"
            :app-id="app.id"
            :app-name="app.name"
            :current="rate"
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
import { DefaultApi, type SubmissionRate } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import SubmissionRateModal from '@/shared/components/submission-rate-modal.vue';
import { describeRefill, describeSustained, formatCount, isDisabled, parseSubmissionRate } from '@/submission-rate';

const { app } = useAppContext();

/** Statuses after which the server definitely did not apply the change. */
const DEFINITIVE_REFUSALS = new Set([400, 401, 403, 404]);

const id = useId();
const timeFormat = new Intl.DateTimeFormat(undefined, { timeStyle: 'medium' });

const rate = shallowRef<SubmissionRate | null>(null);
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
const blocker = computed(() => (isSignedIn.value ? null : 'Sign in again to change the sending limit.'));
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

function summarize(value: SubmissionRate) {
    return isDisabled(value)
        ? 'new messages blocked'
        : `${formatCount(value.per_minute)} per minute with a burst of ${formatCount(value.burst)}`;
}

async function load() {
    if (isUnmounted) return;
    const seq = ++loadSeq;
    isLoading.value = true;
    loadError.value = null;
    try {
        const { data } = await DefaultApi.getSubmissionRate({ path: { appId: app.value.id } });
        if (isUnmounted || seq !== loadSeq) return;
        rate.value = parseSubmissionRate(data);
        loadedAt.value = new Date();
        isStale.value = false;
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        const description = describeApiError(err, {
            notFound: 'The app was not found.',
            unavailable: 'The sending limit is temporarily unavailable. Try again in a moment.'
        });
        loadError.value = rate.value ? { ...description, message: `${description.message} The setting shown is from the last time it loaded.` } : description;
    } finally {
        if (seq === loadSeq) isLoading.value = false;
    }
}

function reload() {
    savedNote.value = null;
    void load();
}

function open() {
    if (!rate.value || blocker.value || isLoading.value || isSubmitting.value) return;
    submitError.value = null;
    savedNote.value = null;
    isModalOpen.value = true;
}

function close() {
    if (isSubmitting.value) return;
    isModalOpen.value = false;
}

async function save(requested: SubmissionRate) {
    if (isSubmitting.value || blocker.value) return;
    const target = app.value;
    isSubmitting.value = true;
    submitError.value = null;
    savedNote.value = null;
    // A newer reload must not overwrite the saved result with an older read.
    loadSeq++;
    isLoading.value = false;

    let saved: SubmissionRate | null = null;
    let outcome: 'succeeded' | 'refused' | 'unknown';
    try {
        const { data } = await DefaultApi.setSubmissionRate({ path: { appId: target.id }, body: requested });
        saved = parseSubmissionRate(data);
        outcome = 'succeeded';
    } catch (err) {
        const status = apiErrorStatus(err);
        const description = describeApiError(err, {
            mutation: true,
            notFound: 'The app was not found. Nothing was changed.',
            unavailable: 'Changing the sending limit is temporarily unavailable. Try again in a moment.'
        });
        if (status !== undefined && DEFINITIVE_REFUSALS.has(status) && !(err instanceof InvalidResponseError)) {
            outcome = 'refused';
            submitError.value = status === 404 ? description : { ...description, message: `${description.message} Nothing was changed.` };
        } else {
            outcome = 'unknown';
            isStale.value = true;
            submitError.value = { ...description, message: `${description.message} It is safe to save the same values again.` };
        }
    } finally {
        isSubmitting.value = false;
    }

    if (isUnmounted) {
        const message =
            outcome === 'succeeded'
                ? `The sending limit for “${target.name}” was saved after you left the page: ${summarize(saved!)}.`
                : outcome === 'unknown'
                  ? `The sending limit change for “${target.name}” may or may not have gone through. Reload the app to check it.`
                  : `The sending limit change for “${target.name}” was refused. Nothing was changed.`;
        showToast({ message, durationSecs: 10 });
        return;
    }

    if (outcome !== 'succeeded' || !saved) return;
    rate.value = saved;
    loadedAt.value = new Date();
    loadError.value = null;
    isStale.value = false;
    isModalOpen.value = false;
    const message = isDisabled(saved)
        ? `New messages blocked for “${target.name}”.`
        : `Sending limit for “${target.name}” set to ${summarize(saved)}.`;
    savedNote.value = `Saved: ${summarize(saved)}.`;
    showToast({ message, durationSecs: 5 });
}

async function confirmLeave() {
    if (!isSubmitting.value) return true;
    return showConfirm(
        'Leave while the sending limit is being saved?',
        'The request has been sent and may still go through. This page won\'t show the result. Reload the app afterwards to check the limit.'
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

.rate-actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
}

.rate-body {
    display: flex;
    flex-direction: column;
    gap: 12px;
    font-size: 13px;
}

.charging {
    summary {
        width: fit-content;
        font-weight: 600;
        cursor: pointer;
    }

    ul {
        display: flex;
        flex-direction: column;
        gap: 4px;
        margin-top: 8px;
        color: var(--text-2);
    }
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
