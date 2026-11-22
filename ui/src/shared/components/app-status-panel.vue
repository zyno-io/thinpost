<template>
    <section :aria-labelledby="`${id}-heading`" class="app-status card" :aria-busy="isSubmitting || isReloading">
        <div class="card-header">
            <h2 :id="`${id}-heading`">
                App status
                <span class="tag" :class="{ success: app.active }">{{ app.active ? 'Active' : 'Paused' }}</span>
            </h2>
            <div class="status-actions">
                <button type="button" class="btn" :disabled="isReloading || isSubmitting || !isSignedIn" @click="reloadStatus">
                    <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isReloading }" aria-hidden="true" />
                    {{ isReloading ? 'Reloading…' : 'Reload status' }}
                </button>
                <button class="btn danger"
                    v-if="app.active"
                    type="button"
                    :disabled="!!blocker || isSubmitting || isReloading"
                    :aria-describedby="blocker ? `${id}-blocker` : undefined"
                    @click="open('pause')"
                >
                    <i class="fa-solid fa-pause" aria-hidden="true" />
                    {{ isSubmitting ? 'Pausing…' : 'Pause app' }}
                </button>
                <button class="btn primary"
                    v-else
                    type="button"
                    :disabled="!!blocker || isSubmitting || isReloading"
                    :aria-describedby="blocker ? `${id}-blocker` : undefined"
                    @click="open('resume')"
                >
                    <i class="fa-solid fa-play" aria-hidden="true" />
                    {{ isSubmitting ? 'Resuming…' : 'Resume app' }}
                </button>
            </div>
        </div>

        <div class="card-body status-body">
            <p v-if="app.active">Not paused. Whether new messages are accepted depends on the sending limit below.</p>
            <p v-else>Paused: nothing new is accepted or sent. API keys and queued messages are kept.</p>
            <p class="hint">
                As of <time :datetime="loadedAt.toISOString()">{{ formatTime(loadedAt) }}</time>
            </p>

            <p v-if="blocker" :id="`${id}-blocker`" class="blocker">{{ blocker }}</p>

            <div v-if="showPauseNotice && !app.active" class="notice warning" role="status">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <div class="notice-body">
                    <strong>Paused. Sends already under way may still finish.</strong>
                    <HelpButton topic="pausing">
                        <p>
                            A message that was already being sent when you paused can still go out, and a webhook event already on its way can still
                            arrive. Pausing can't recall them.
                        </p>
                    </HelpButton>
                </div>
            </div>

            <div v-if="statusNote" class="notice warning" role="status">
                <i class="fa-solid fa-circle-info" aria-hidden="true" />
                <p class="notice-body">{{ statusNote }}</p>
            </div>

            <ApiErrorNotice v-if="actionError" :error="actionError" :retry="needsReload && isSignedIn ? reloadStatus : undefined" retry-label="Reload status" />
            <ApiErrorNotice v-if="reloadError" :error="reloadError" :retry="isSignedIn ? reloadStatus : undefined" retry-label="Reload status" />
        </div>

        <AppStatusModal
            v-if="pendingAction"
            :action="pendingAction"
            :app-id="app.id"
            :app-name="app.name"
            :shown-active="app.active"
            :loaded-at="loadedAt"
            :is-submitting="isSubmitting"
            :blocker="blocker"
            @close="close"
            @submit="confirm"
        />
    </section>
</template>

<script setup lang="ts">
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, useId } from 'vue';
import { onBeforeRouteLeave, onBeforeRouteUpdate } from 'vue-router';

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription, type DescribeOptions } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState, registerLeaveGuard } from '@/auth';
import { DefaultApi } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import AppStatusModal, { type AppStatusAction } from '@/shared/components/app-status-modal.vue';

const context = useAppContext();
const { app, loadedAt } = context;

/** Statuses after which the server definitely did not apply the change. */
const DEFINITIVE_REFUSALS = new Set([400, 401, 403, 404, 409]);

const ERRORS: Record<AppStatusAction, DescribeOptions> = {
    pause: {
        mutation: true,
        notFound: 'The app was not found. Nothing was paused.',
        conflict: 'Thinpost couldn\'t pause the app because its state just changed. Nothing was changed.',
        unavailable: 'Pausing is temporarily unavailable. Try again in a moment.'
    },
    resume: {
        mutation: true,
        notFound: 'The app was not found. Nothing was resumed.',
        conflict: 'Thinpost couldn\'t resume the app because its state just changed. Nothing was changed.',
        unavailable: 'Resuming is temporarily unavailable. Try again in a moment.'
    }
};

const id = useId();
const timeFormat = new Intl.DateTimeFormat(undefined, { timeStyle: 'medium' });

const pendingAction = ref<AppStatusAction | null>(null);
const isSubmitting = ref(false);
const isReloading = ref(false);
const actionError = ref<ApiErrorDescription | null>(null);
const reloadError = ref<ApiErrorDescription | null>(null);
const statusNote = ref<string | null>(null);
const showPauseNotice = ref(false);
/** An earlier request has an unknown outcome; changes stay blocked until a reload succeeds. */
const needsReload = ref(false);
/** The last reload found no such app. */
const isMissing = ref(false);

const isSignedIn = computed(() => authState.status === 'signed-in');

/** Why a status change is not offered, from what this page knows; the server re-checks everything. */
const blocker = computed(() => {
    if (!isSignedIn.value) return 'Sign in again to change the app status.';
    if (needsReload.value) return 'Reload the status first: an earlier request may or may not have gone through.';
    if (isMissing.value) return 'The app was not found the last time its status was loaded.';
    return null;
});

let reloadSeq = 0;
let isUnmounted = false;

function formatTime(date: Date) {
    return timeFormat.format(date);
}

function verb(action: AppStatusAction) {
    return action === 'pause' ? 'pause' : 'resume';
}

function open(action: AppStatusAction) {
    if (blocker.value || isSubmitting.value || isReloading.value) return;
    if ((action === 'pause') !== app.value.active) return;
    actionError.value = null;
    statusNote.value = null;
    pendingAction.value = action;
}

/** Reload requested by the user; never carries an expected state. */
function reloadStatus() {
    void reload();
}

function close() {
    if (isSubmitting.value) return;
    pendingAction.value = null;
}

/**
 * Reloads the app from the list endpoint. Only a successful reload clears an unknown outcome.
 * `expected` is the state a just-confirmed change produced; it is shown if the reload itself fails.
 */
async function reload(expected?: { active: boolean; action: AppStatusAction }) {
    if (isUnmounted) return;
    const seq = ++reloadSeq;
    isReloading.value = true;
    reloadError.value = null;
    try {
        const found = await context.reload();
        if (isUnmounted || seq !== reloadSeq) return;
        if (needsReload.value) {
            needsReload.value = false;
            actionError.value = null;
        }
        if (!found) {
            isMissing.value = true;
            reloadError.value = { kind: 'not-found', message: 'The app was not found. The status shown is from the last time it loaded.' };
            return;
        }
        isMissing.value = false;
        if (expected && found.active !== expected.active) {
            statusNote.value = `The ${verb(expected.action)} request worked, but the app now shows as ${found.active ? 'active' : 'inactive'}. Someone else may have changed it since.`;
        }
    } catch (err) {
        if (isUnmounted || seq !== reloadSeq) return;
        const description = describeApiError(err, { notFound: 'The app was not found.' });
        if (expected) {
            // The 204 confirmed the state at that moment; show it rather than the older load.
            context.update({ ...app.value, active: expected.active });
            reloadError.value = {
                ...description,
                message: `The ${verb(expected.action)} request worked, but the status couldn't be reloaded to confirm it. ${description.message}`
            };
        } else {
            reloadError.value = {
                ...description,
                message: `${description.message} The status shown is from the last time it loaded.${needsReload.value ? ' You can\'t change the status until a reload succeeds.' : ''}`
            };
        }
    } finally {
        if (seq === reloadSeq) isReloading.value = false;
    }
}

async function confirm() {
    const action = pendingAction.value;
    const target = app.value;
    // The session may have ended, or the status been reloaded, while the dialog was open.
    if (!action || isSubmitting.value || blocker.value || (action === 'pause') !== target.active) {
        if (!isSubmitting.value) pendingAction.value = null;
        return;
    }
    const active = action === 'resume';
    isSubmitting.value = true;
    actionError.value = null;
    reloadError.value = null;
    statusNote.value = null;
    showPauseNotice.value = false;

    let outcome: 'succeeded' | 'refused' | 'unknown';
    try {
        const request = active ? DefaultApi.activateApp : DefaultApi.deactivateApp;
        const response = await request({ path: { appId: target.id } });
        if (response?.response?.status !== 204) throw new InvalidResponseError();
        outcome = 'succeeded';
    } catch (err) {
        const status = apiErrorStatus(err);
        const description = describeApiError(err, ERRORS[action]);
        if (status !== undefined && DEFINITIVE_REFUSALS.has(status) && !(err instanceof InvalidResponseError)) {
            outcome = 'refused';
            const suffix = status === 404 || status === 409 ? '' : ' Nothing was changed.';
            actionError.value = { ...description, message: description.message + suffix };
        } else {
            // No definitive answer: the change may have been committed. Never replay automatically.
            outcome = 'unknown';
            needsReload.value = true;
            actionError.value = description;
        }
    } finally {
        isSubmitting.value = false;
        pendingAction.value = null;
    }

    if (isUnmounted) {
        // The user confirmed leaving mid-request; report the outcome without request details.
        const message =
            outcome === 'succeeded'
                ? `“${target.name}” was ${active ? 'resumed' : 'paused'} after you left the page.`
                : outcome === 'unknown'
                  ? `The ${verb(action)} request for “${target.name}” may or may not have gone through. Reload the app to check its status.`
                  : `The ${verb(action)} request for “${target.name}” was refused. Nothing was changed.`;
        showToast({ message, durationSecs: 10 });
        return;
    }

    if (outcome === 'succeeded') {
        showPauseNotice.value = !active;
        showToast({
            message: active
                ? `Resumed “${target.name}”. Its API keys work again and queued messages can be sent, subject to other limits.`
                : `Paused “${target.name}”. Sends already under way may still finish.`,
            durationSecs: 6
        });
        if (isSignedIn.value) {
            await reload({ active, action });
        } else {
            context.update({ ...target, active });
            reloadError.value = {
                kind: 'unauthenticated',
                message: `The ${verb(action)} request worked, but your session ended before the status could be reloaded. Sign in again to check it.`
            };
        }
    } else if (outcome === 'refused' && isSignedIn.value && (actionError.value?.status === 404 || actionError.value?.status === 409)) {
        // Definitive refusal because the app changed under us; show its current state.
        await reload();
    }
}

async function confirmLeave() {
    if (!isSubmitting.value) return true;
    return showConfirm(
        'Leave while the app status is changing?',
        'The request has been sent and may still go through. This page won\'t show the result. Reload the app afterwards to check its status.'
    );
}

onBeforeRouteLeave(confirmLeave);
onBeforeRouteUpdate(confirmLeave);
// Sign-in and sign-out leave the page without a route change.
const unregisterLeaveGuard = registerLeaveGuard(confirmLeave);

onBeforeUnmount(() => {
    isUnmounted = true;
    unregisterLeaveGuard();
});
</script>

<style lang="scss" scoped>
.card-header h2 {
    display: flex;
    align-items: center;
    gap: 8px;
}

.status-actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
}

.status-body {
    display: flex;
    flex-direction: column;
    gap: 10px;
    font-size: 13px;
}

.blocker {
    color: var(--text-2);
}
</style>
