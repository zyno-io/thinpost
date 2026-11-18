<template>
    <section aria-labelledby="deliveries-heading" class="card history">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 id="deliveries-heading">
                    Webhook event history
                </h2>
                <HelpButton topic="webhook event history">
                    <p>
                        Newest first, one row per event and webhook. Rows show only when and whether an event was sent. They never include the event
                        payload, message contents or signing secrets.
                    </p>
                    <p>Each page of history and each list of attempts you open is recorded in the audit log.</p>
                    <p>
                        “Queued” means the event is waiting to be sent. It isn't a promise that it will arrive right away.
                    </p>
                </HelpButton>
            </div>
            <button v-if="deliveries" type="button" class="btn sm" :disabled="isRefreshing || isReplaying" @click="refresh">
                <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isRefreshing }" aria-hidden="true" />
                {{ isRefreshing ? 'Refreshing…' : 'Refresh' }}
            </button>
        </div>

        <div class="card-body history-body">
        <div v-if="runtimeUnavailable" class="notice warning">
            <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
            <p class="notice-body">Webhooks are unavailable right now, so events can't be sent again.</p>
        </div>

        <ApiErrorNotice v-if="actionError" :error="actionError" :retry="needsReload ? refresh : undefined" />

        <Loader v-if="isLoading" label="Loading webhook event history" />

        <ApiErrorNotice v-else-if="loadError" :error="loadError" :retry="load" />

        <template v-else-if="deliveries">
            <ApiErrorNotice v-if="refreshError" :error="refreshError" :retry="refresh" />

            <div v-if="!deliveries.length" class="empty">
                <i class="fa-solid fa-clock-rotate-left" aria-hidden="true" />
                <h2>No webhook events yet</h2>
            </div>
        </template>
        </div>

        <template v-if="!isLoading && !loadError && deliveries && deliveries.length">
                <div class="table-wrap" :aria-busy="isRefreshing">
                    <table class="data">
                        <caption class="sr-only">Webhook events, newest first</caption>
                        <thead>
                            <tr>
                                <th scope="col">Event</th>
                                <th scope="col">Message result</th>
                                <th scope="col">Webhook</th>
                                <th scope="col">Status</th>
                                <th scope="col" class="num" title="Attempts in the current automatic retry cycle. Starts over when you send the event again.">Attempts</th>
                                <th scope="col"><span class="sr-only">Actions</span></th>
                            </tr>
                        </thead>
                        <tbody>
                            <template v-for="delivery in deliveries" :key="keyOf(delivery)">
                                <tr :data-delivery="keyOf(delivery)">
                                    <th scope="row" class="event-cell">
                                        <time :datetime="delivery.occurred_at">{{ formatDateTime(delivery.occurred_at) }}</time>
                                        <span class="sub">ID <code>{{ delivery.event_id }}</code></span>
                                    </th>
                                    <td>
                                        <code class="outcome">{{ delivery.outcome }}</code>
                                        <span v-if="delivery.message_id" class="sub">
                                            <RouterLink :to="{ name: 'app-message', params: { appId, messageId: delivery.message_id } }">
                                                Message <code>{{ delivery.message_id }}</code>
                                            </RouterLink>
                                        </span>
                                        <span v-else-if="delivery.inbound_id" class="sub">
                                            <template v-if="inboundId">
                                                Inbound message <code>{{ delivery.inbound_id }}</code>
                                            </template>
                                            <RouterLink v-else :to="{ name: 'app-inbound-message', params: { appId, inboundId: delivery.inbound_id } }">
                                                Inbound message <code>{{ delivery.inbound_id }}</code>
                                            </RouterLink>
                                        </span>
                                    </td>
                                    <td class="subscription-cell">
                                        <code v-if="endpointOf(delivery)" class="endpoint">{{ endpointOf(delivery) }}</code>
                                        <span v-else-if="subscriptions" class="deleted">Deleted webhook</span>
                                        <span class="sub">ID <code>{{ delivery.subscription_id }}</code></span>
                                    </td>
                                    <td class="state-cell">
                                        <span
                                            class="tag"
                                            :class="tagClass(webhookDeliveryState(delivery.state).tone)"
                                            :title="webhookDeliveryState(delivery.state).description"
                                        >
                                            {{ webhookDeliveryState(delivery.state).label }}
                                        </span>
                                        <span v-if="delivery.delivered_at" class="sub">
                                            at <time :datetime="delivery.delivered_at">{{ formatDateTime(delivery.delivered_at) }}</time>
                                        </span>
                                        <span v-else-if="delivery.next_attempt_at" class="sub">
                                            not before <time :datetime="delivery.next_attempt_at">{{ formatDateTime(delivery.next_attempt_at) }}</time>
                                        </span>
                                    </td>
                                    <td class="num">{{ delivery.attempts }}</td>
                                    <td class="actions">
                                        <div class="action-buttons">
                                            <button class="btn sm"
                                                type="button"
                                                :aria-expanded="expanded?.key === keyOf(delivery)"
                                                :aria-controls="expanded?.key === keyOf(delivery) ? `${id}-attempts` : undefined"
                                                @click="toggleAttempts(delivery)"
                                            >
                                                {{ expanded?.key === keyOf(delivery) ? 'Hide attempts' : 'Attempts' }}
                                                <span class="sr-only">for event {{ delivery.event_id }}</span>
                                            </button>
                                            <button class="btn sm"
                                                v-if="delivery.state === 'failed'"
                                                type="button"
                                                :disabled="!!replayBlocker(delivery) || !canReplay"
                                                :aria-describedby="replayBlocker(delivery) ? `${id}-blocker-${keyOf(delivery)}` : undefined"
                                                @click="openReplay(delivery)"
                                            >
                                                Send again
                                                <span class="sr-only">event {{ delivery.event_id }}</span>
                                            </button>
                                        </div>
                                        <p
                                            v-if="delivery.state === 'failed' && replayBlocker(delivery)"
                                            :id="`${id}-blocker-${keyOf(delivery)}`"
                                            class="blocker"
                                        >
                                            {{ replayBlocker(delivery) }}
                                        </p>
                                    </td>
                                </tr>
                                <tr v-if="expanded?.key === keyOf(delivery)" class="attempts-row">
                                    <td :id="`${id}-attempts`" colspan="6">
                                        <div class="attempts" role="region" :aria-label="`Attempts for event ${delivery.event_id}`">
                                            <Loader v-if="expanded.status === 'loading'" label="Loading attempts" />
                                            <ApiErrorNotice
                                                v-else-if="expanded.status === 'error' && expanded.error"
                                                :error="expanded.error"
                                                :retry="() => loadAttempts(delivery)"
                                            />
                                            <template v-else-if="expanded.status === 'loaded'">
                                                <p v-if="!expanded.attempts.length" class="hint">No attempts recorded yet.</p>
                                                <template v-else>
                                                    <p v-if="expanded.truncated" class="hint">
                                                        Showing only the latest {{ expanded.attempts.length }} attempts.
                                                    </p>
                                                    <table class="data attempts-table">
                                                        <caption class="sr-only">Attempts for event {{ delivery.event_id }}, newest first</caption>
                                                        <thead>
                                                            <tr>
                                                                <th scope="col">Started</th>
                                                                <th scope="col">Result</th>
                                                                <th scope="col" title="0 means no response was received">Response status</th>
                                                                <th scope="col">Finished</th>
                                                            </tr>
                                                        </thead>
                                                        <tbody>
                                                            <tr v-for="(attempt, index) in expanded.attempts" :key="index">
                                                                <td class="when">
                                                                    <time :datetime="attempt.started_at">{{ formatDateTime(attempt.started_at) }}</time>
                                                                </td>
                                                                <td>
                                                                    <span
                                                                        class="tag"
                                                                        :class="tagClass(webhookAttemptState(attempt.state).tone)"
                                                                        :title="webhookAttemptState(attempt.state).description"
                                                                    >
                                                                        {{ webhookAttemptState(attempt.state).label }}
                                                                    </span>
                                                                </td>
                                                                <td>{{ httpStatusLabel(attempt.http_status) }}</td>
                                                                <td class="when">
                                                                    <time v-if="attempt.finished_at" :datetime="attempt.finished_at">
                                                                        {{ formatDateTime(attempt.finished_at) }}
                                                                    </time>
                                                                    <span v-else class="muted">Not recorded</span>
                                                                </td>
                                                            </tr>
                                                        </tbody>
                                                    </table>
                                                </template>
                                            </template>
                                        </div>
                                    </td>
                                </tr>
                            </template>
                        </tbody>
                    </table>
                </div>

                <div class="card-body history-body">
                <ApiErrorNotice v-if="moreError" :error="moreError" :retry="loadMore" />

                <div class="pager">
                    <span class="muted" role="status">
                        {{ deliveries.length }} {{ deliveries.length === 1 ? 'event' : 'events' }} loaded{{ nextCursor ? '' : ' · end of history' }}
                    </span>
                    <button v-if="nextCursor" type="button" class="btn" :disabled="isLoadingMore || isRefreshing" @click="loadMore">
                        {{ isLoadingMore ? 'Loading…' : 'Load older events' }}
                    </button>
                </div>
                </div>
        </template>

        <ReplayModal
            v-if="replayTarget"
            :event-id="replayTarget.event_id"
            :subscription-id="replayTarget.subscription_id"
            :endpoint="endpointOf(replayTarget)"
            :is-submitting="isReplaying"
            @close="closeReplay"
            @submit="confirmReplay"
        />
    </section>
</template>

<script setup lang="ts">
import { showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, shallowRef, useId, watch } from 'vue';

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription, type DescribeOptions } from '@/api-errors';
import { authState } from '@/auth';
import { formatDateTime } from '@/format';
import { DefaultApi, type WebhookAttempt, type WebhookDelivery } from '@/openapi-client-generated';
import { tagClass, webhookAttemptState, webhookDeliveryState } from '@/states';
import { isOpaqueId } from '@/validation';

import ApiErrorNotice from './api-error-notice.vue';
import HelpButton from './help-button.vue';
import Loader from './loader.vue';
import ReplayModal from './replay-modal.vue';

/** What the webhook list says about a webhook; the server re-checks all of it on replay. */
export interface SubscriptionSummary {
    endpoint: string;
    /** ready: enabled; disabled: not enabled. An endpoint missing from the list was deleted. */
    status: 'ready' | 'disabled';
}

const props = defineProps<{
    appId: string;
    appActive: boolean;
    /** Endpoints by ID; null while the endpoint list is not loaded or failed to load. */
    subscriptions: ReadonlyMap<string, SubscriptionSummary> | null;
    /** Webhook administration answered HTTP 503. */
    runtimeUnavailable: boolean;
    /** Show only the events of one received message (its detail page) instead of the app's whole history. */
    inboundId?: string;
}>();

const emit = defineEmits<{
    /** The app or an endpoint may have changed; the endpoint list should be reloaded. */
    changed: [];
}>();

interface AttemptsView {
    key: string;
    status: 'loading' | 'loaded' | 'error';
    attempts: WebhookAttempt[];
    truncated: boolean;
    error: ApiErrorDescription | null;
}

interface Page {
    deliveries: WebhookDelivery[];
    nextCursor: string | null;
}

const PAGE_SIZE = 50;
const MAX_CURSOR_LENGTH = 512;
const MAX_ATTEMPTS = 50;
const DELIVERY_STATES = new Set(['queued', 'in_flight', 'delivered', 'failed', 'cancelled', 'recovery_hold']);
const ATTEMPT_STATES = new Set(['in_flight', 'succeeded', 'failed', 'unknown', 'content_unavailable', 'deferred']);

const UNAVAILABLE = 'Webhook event history is unavailable right now. Try again in a moment.';
const NOT_FOUND = 'The app was not found, or webhook event history isn\'t available.';

const REPLAY_ERRORS: DescribeOptions = {
    mutation: true,
    notFound: 'This event or its webhook wasn\'t found. The webhook may have been deleted. Nothing was queued.',
    conflict:
        'The event wasn\'t sent again, and nothing was queued. It is no longer marked failed (it may already be queued or on its way), the app is paused, the webhook is disabled or deleted, or the received message has been kept for its full retention period and removed. The history and webhooks have been reloaded.',
    unavailable: 'Sending events again is unavailable right now. Try again in a moment.'
};

const id = useId();
const deliveries = shallowRef<WebhookDelivery[] | null>(null);
const nextCursor = ref<string | null>(null);
const isLoading = ref(true);
const loadError = ref<ApiErrorDescription | null>(null);
const isLoadingMore = ref(false);
const moreError = ref<ApiErrorDescription | null>(null);
const isRefreshing = ref(false);
const refreshError = ref<ApiErrorDescription | null>(null);
const expanded = shallowRef<AttemptsView | null>(null);
const replayTarget = shallowRef<WebhookDelivery | null>(null);
const isReplaying = ref(false);
const actionError = ref<ApiErrorDescription | null>(null);
/** A replay's outcome is unknown; replay stays blocked until the history has been reloaded. */
const needsReload = ref(false);

const isSignedIn = computed(() => authState.status === 'signed-in');
const canReplay = computed(() => isSignedIn.value && !isReplaying.value && !isRefreshing.value && !isLoading.value);

let loadSeq = 0;
let attemptsSeq = 0;
let isUnmounted = false;
onBeforeUnmount(() => {
    isUnmounted = true;
});

watch([() => props.appId, () => props.inboundId], load, { immediate: true });

function keyOf(delivery: Pick<WebhookDelivery, 'event_id' | 'subscription_id'>): string {
    return `${delivery.event_id}-${delivery.subscription_id}`;
}

function endpointOf(delivery: WebhookDelivery): string | null {
    return props.subscriptions?.get(delivery.subscription_id)?.endpoint ?? null;
}

function isOptionalTime(value: unknown): value is string | undefined {
    return value === undefined || typeof value === 'string';
}

function toDelivery(value: unknown): WebhookDelivery | null {
    const v = value as Partial<WebhookDelivery> | null;
    if (
        !v ||
        !isOpaqueId(v.event_id) ||
        !isOpaqueId(v.subscription_id) ||
        // Exactly one source: an outbound message, or an inbound message.
        (v.message_id === undefined) === (v.inbound_id === undefined) ||
        (v.message_id !== undefined && !isOpaqueId(v.message_id)) ||
        (v.inbound_id !== undefined && !isOpaqueId(v.inbound_id)) ||
        (v.delivery_id !== undefined && !isOpaqueId(v.delivery_id)) ||
        typeof v.outcome !== 'string' ||
        typeof v.state !== 'string' ||
        !DELIVERY_STATES.has(v.state) ||
        !Number.isInteger(v.attempts) ||
        (v.attempts as number) < 0 ||
        typeof v.occurred_at !== 'string' ||
        !isOptionalTime(v.next_attempt_at) ||
        !isOptionalTime(v.delivered_at)
    ) {
        return null;
    }
    // Keep only contract fields so nothing unexpected in a response is retained.
    return {
        event_id: v.event_id,
        subscription_id: v.subscription_id,
        message_id: v.message_id,
        inbound_id: v.inbound_id,
        delivery_id: v.delivery_id,
        outcome: v.outcome,
        state: v.state,
        attempts: v.attempts as number,
        occurred_at: v.occurred_at,
        next_attempt_at: v.next_attempt_at,
        delivered_at: v.delivered_at
    };
}

function toAttempt(value: unknown): WebhookAttempt | null {
    const v = value as Partial<WebhookAttempt> | null;
    if (
        !v ||
        typeof v.state !== 'string' ||
        !ATTEMPT_STATES.has(v.state) ||
        typeof v.started_at !== 'string' ||
        !isOptionalTime(v.finished_at) ||
        (v.http_status !== undefined && (!Number.isInteger(v.http_status) || v.http_status < 0 || v.http_status > 599))
    ) {
        return null;
    }
    return { state: v.state, http_status: v.http_status, started_at: v.started_at, finished_at: v.finished_at };
}

async function fetchPage(cursor?: string): Promise<Page> {
    const appId = props.appId;
    const query = cursor ? { limit: PAGE_SIZE, cursor } : { limit: PAGE_SIZE };
    const { data } = props.inboundId
        ? await DefaultApi.listInboundMessageWebhookDeliveries({ path: { appId, inboundId: props.inboundId }, query })
        : await DefaultApi.listWebhookDeliveries({ path: { appId }, query });
    const raw: unknown = data?.deliveries;
    if (!Array.isArray(raw)) throw new InvalidResponseError();
    const items = raw.map(toDelivery);
    if (items.some(item => item === null)) throw new InvalidResponseError();
    const next: unknown = data?.next_cursor;
    if (next !== undefined && (typeof next !== 'string' || next.length > MAX_CURSOR_LENGTH)) throw new InvalidResponseError();
    return { deliveries: items as WebhookDelivery[], nextCursor: next || null };
}

function describeListError(err: unknown): ApiErrorDescription {
    return describeApiError(err, { notFound: NOT_FOUND, unavailable: UNAVAILABLE });
}

function resetTransient() {
    isLoadingMore.value = false;
    moreError.value = null;
    refreshError.value = null;
    attemptsSeq++;
    expanded.value = null;
}

async function load() {
    const seq = ++loadSeq;
    isLoading.value = true;
    isRefreshing.value = false;
    loadError.value = null;
    actionError.value = null;
    replayTarget.value = null;
    deliveries.value = null;
    nextCursor.value = null;
    resetTransient();

    if (!isOpaqueId(props.appId) || (props.inboundId !== undefined && !isOpaqueId(props.inboundId))) {
        loadError.value = { kind: 'not-found', message: NOT_FOUND };
        isLoading.value = false;
        return;
    }

    try {
        const page = await fetchPage();
        if (seq !== loadSeq) return;
        deliveries.value = page.deliveries;
        nextCursor.value = page.nextCursor;
        needsReload.value = false;
    } catch (err) {
        if (seq === loadSeq) loadError.value = describeListError(err);
    } finally {
        if (seq === loadSeq) isLoading.value = false;
    }
}

/** Reloads the first page while keeping the current rows on screen. */
async function refresh() {
    if (isRefreshing.value) return;
    // Endpoints added since the page loaded must not show as deleted.
    emit('changed');
    if (!deliveries.value) return load();
    // Invalidates any in-flight older page; its rows would belong to the previous listing.
    const seq = ++loadSeq;
    isRefreshing.value = true;
    resetTransient();
    try {
        const page = await fetchPage();
        if (seq !== loadSeq) return;
        deliveries.value = page.deliveries;
        nextCursor.value = page.nextCursor;
        if (needsReload.value) {
            // The reloaded rows now show whether the uncertain replay took effect.
            needsReload.value = false;
            actionError.value = null;
        }
    } catch (err) {
        if (seq === loadSeq) {
            const description = describeListError(err);
            refreshError.value = { ...description, message: `${description.message} The rows below are from the last time they loaded.` };
        }
    } finally {
        if (seq === loadSeq) isRefreshing.value = false;
    }
}

async function loadMore() {
    const cursor = nextCursor.value;
    if (!cursor || isLoadingMore.value || !deliveries.value) return;
    const seq = loadSeq;
    isLoadingMore.value = true;
    moreError.value = null;
    try {
        const page = await fetchPage(cursor);
        if (seq !== loadSeq) return;
        // Keyset pages do not overlap, but guard against duplicates if rows shift between requests.
        const seen = new Set((deliveries.value ?? []).map(keyOf));
        deliveries.value = [...(deliveries.value ?? []), ...page.deliveries.filter(d => !seen.has(keyOf(d)))];
        nextCursor.value = page.nextCursor;
    } catch (err) {
        // Keep the loaded rows and the cursor so the same page can be retried.
        if (seq === loadSeq) moreError.value = describeListError(err);
    } finally {
        if (seq === loadSeq) isLoadingMore.value = false;
    }
}

function toggleAttempts(delivery: WebhookDelivery) {
    if (expanded.value?.key === keyOf(delivery)) {
        attemptsSeq++;
        expanded.value = null;
        return;
    }
    void loadAttempts(delivery);
}

async function loadAttempts(delivery: WebhookDelivery) {
    const key = keyOf(delivery);
    const seq = loadSeq;
    const thisLoad = ++attemptsSeq;
    const isCurrent = () => !isUnmounted && seq === loadSeq && thisLoad === attemptsSeq;
    expanded.value = { key, status: 'loading', attempts: [], truncated: false, error: null };
    try {
        const { data } = await DefaultApi.listWebhookDeliveryAttempts({
            path: { appId: props.appId, eventId: delivery.event_id, subscriptionId: delivery.subscription_id }
        });
        if (!isCurrent()) return;
        const raw: unknown = data?.attempts;
        const truncated: unknown = data?.truncated;
        if (!Array.isArray(raw) || raw.length > MAX_ATTEMPTS || typeof truncated !== 'boolean') throw new InvalidResponseError();
        const attempts = raw.map(toAttempt);
        if (attempts.some(item => item === null)) throw new InvalidResponseError();
        expanded.value = { key, status: 'loaded', attempts: attempts as WebhookAttempt[], truncated, error: null };
    } catch (err) {
        if (!isCurrent()) return;
        const error = describeApiError(err, {
            notFound: 'This event wasn\'t found for this app.',
            unavailable: UNAVAILABLE
        });
        expanded.value = { key, status: 'error', attempts: [], truncated: false, error };
    }
}

function httpStatusLabel(status: number | undefined): string {
    if (status === undefined) return 'None recorded';
    if (status === 0) return '0 · no response';
    return String(status);
}

/** Why replay is not offered, from what this page knows; the server re-checks everything. */
function replayBlocker(delivery: WebhookDelivery): string | null {
    if (delivery.state !== 'failed') return 'Only failed events can be sent again.';
    if (needsReload.value) return 'Reload the history first: an earlier attempt to send this again may or may not have gone through.';
    if (!props.appActive) return 'The app is paused.';
    if (props.runtimeUnavailable) return 'Webhooks are unavailable right now.';
    if (!props.subscriptions) return 'The webhook list hasn\'t loaded, so the webhook can\'t be checked.';
    const endpoint = props.subscriptions.get(delivery.subscription_id);
    if (!endpoint) return 'This webhook was deleted.';
    if (endpoint.status === 'disabled') return 'The webhook is disabled.';
    return null;
}

function openReplay(delivery: WebhookDelivery) {
    if (!canReplay.value || replayBlocker(delivery)) return;
    actionError.value = null;
    replayTarget.value = delivery;
}

function closeReplay() {
    if (isReplaying.value) return;
    replayTarget.value = null;
}

async function confirmReplay() {
    const target = replayTarget.value;
    // The session may have ended, or the endpoint list changed, while the dialog was open.
    if (!target || !canReplay.value || replayBlocker(target)) {
        replayTarget.value = null;
        return;
    }
    const seq = loadSeq;
    const appId = props.appId;
    isReplaying.value = true;
    actionError.value = null;
    let reload = false;
    try {
        const response = await DefaultApi.replayWebhookDelivery({
            path: { appId, eventId: target.event_id, subscriptionId: target.subscription_id }
        });
        if (isUnmounted || appId !== props.appId) return;
        if (response?.response?.status !== 204) throw new InvalidResponseError();
        showToast({ message: 'Queued to send again. Your receiver should ignore repeats by event ID.', durationSecs: 6 });
        reload = true;
    } catch (err) {
        if (isUnmounted || appId !== props.appId) return;
        const status = apiErrorStatus(err);
        actionError.value = describeApiError(err, REPLAY_ERRORS);
        if (status === 404 || status === 409) {
            // Definitive refusal; the delivery or its endpoint changed under us. The refresh below also reloads the endpoints.
            reload = true;
        } else if (status === undefined || status >= 500 || err instanceof InvalidResponseError) {
            // No definitive answer: the replay may have been committed. Block repeats until reloaded.
            needsReload.value = true;
        }
    } finally {
        isReplaying.value = false;
        replayTarget.value = null;
    }
    if (reload && seq === loadSeq && !isUnmounted) await refresh();
}
</script>

<style lang="scss" scoped>
.history-body {
    display: flex;
    flex-direction: column;
    gap: 12px;
}

/* Hide the body when every child rendered nothing (only v-if comments remain). */
.history-body.history-body:not(:has(*)) {
    display: none;
}

table.data th.event-cell {
    white-space: nowrap;
}

.sub {
    display: block;
    margin-top: 2px;
    color: var(--text-3);
    font-size: 12px;
    overflow-wrap: anywhere;

    code {
        font-size: 12px;
    }
}

.subscription-cell {
    min-width: 192px;
}

.deleted {
    color: var(--text-3);
    font-style: italic;
}

.endpoint {
    display: block;
    font-weight: 600;
    overflow-wrap: anywhere;
}

.state-cell {
    white-space: nowrap;
}

.actions {
    text-align: right;
}

.action-buttons {
    display: flex;
    justify-content: flex-end;
    gap: 8px;
    white-space: nowrap;
}

.blocker {
    max-width: 224px;
    margin: 4px 0 0 auto;
}

.attempts-row > td {
    background: var(--surface-2);
}

.attempts {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 4px 0;
}

table.data.attempts-table {
    border: 1px solid var(--border);
    border-radius: var(--radius);
}

.when {
    white-space: nowrap;
}

.truncated {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--warning);
    font-size: 12px;
    font-weight: 600;
}
</style>
