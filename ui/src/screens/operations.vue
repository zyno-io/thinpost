<template>
    <div id="operations">
        <div class="header">
            <div class="heading-with-help">
                <h1>Operations</h1>
                <HelpButton topic="operations">
                    <p>A read-only snapshot of how Thinpost is handling mail right now: whether mail is being accepted, whether sending workers are connected, what is waiting in the queues and what needs attention. It keeps working while mail intake is closed, but can briefly be unavailable during an upgrade.</p>
                    <p>It shows counts and IDs only, never addresses or subjects. Opening or closing mail intake and recovery are done by your Thinpost administrator, not here. This page refreshes every 30 seconds while the tab is visible.</p>
                </HelpButton>
            </div>
            <div class="header-actions">
                <span v-if="ops" class="muted asof" role="status">as of <time :datetime="ops.generated_at">{{ formatDateTime(ops.generated_at) }}</time></span>
                <button type="button" class="btn sm" :disabled="isLoading" @click="refresh()">
                    <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isLoading && !!ops }" aria-hidden="true" />Refresh
                </button>
            </div>
        </div>

        <Loader v-if="isLoading && !ops && !error" label="Loading Thinpost status" />

        <div v-if="error" class="notice danger" role="alert" data-testid="ops-error">
            <i class="fa-solid fa-circle-exclamation" aria-hidden="true" />
            <div class="notice-body">
                <p><strong>{{ error.kind === 'unavailable' ? 'Thinpost status is unavailable.' : 'Thinpost status couldn\'t be loaded.' }}</strong> {{ error.message }}</p>
            </div>
            <div class="notice-actions"><button type="button" class="btn sm" @click="refresh()">Try again</button></div>
        </div>

        <template v-if="ops">
            <section class="card" aria-labelledby="ops-gate-heading">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="ops-gate-heading">
                            Mail intake
                            <span class="tag" :class="gateTag.cls" data-testid="gate-tag">{{ gateTag.label }}</span>
                        </h2>
                        <HelpButton topic="mail intake">
                            <p>Thinpost accepts and sends mail only while mail intake is open. Upgrades close it while data is migrated and reopen it afterwards. Recovery means Thinpost is recovering after a database restore.</p>
                            <p>Your Thinpost administrator opens and closes it. This console can't.</p>
                        </HelpButton>
                    </div>
                </div>
                <div class="card-body rows">
                    <p class="line">
                        <span>Change #{{ ops.gate.revision }}</span>
                        <span v-if="ops.gate.changed_by">· {{ ops.gate.mode === 'open' ? 'changed' : ops.gate.mode === 'recovery' ? 'set' : 'closed' }} by <code>{{ ops.gate.changed_by }}</code></span>
                        <span>· since <time :datetime="ops.gate.changed_at">{{ formatDateTime(ops.gate.changed_at) }}</time></span>
                        <span v-if="ops.gate.mode === 'recovery' && !ops.gate.quarantine_complete" class="tag warning" title="Thinpost is recovering from a database restore and the safety check hasn't finished.">Restore check not completed</span>
                    </p>
                    <div v-if="staleClosure" class="notice warning" role="status" data-testid="stale-closure">
                        <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                        <p class="notice-body">Closed for an upgrade for {{ formatAge(ops.gate.changed_at, now) }}. The upgrade may be stuck. Tell your Thinpost administrator if it doesn't finish.</p>
                    </div>
                    <div v-if="!ops.gate.generation_matches" class="notice warning" role="status" data-testid="generation-mismatch">
                        <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                        <p class="notice-body">This Thinpost server's deployment ID doesn't match the one mail intake is set for, so this server can't accept or send mail. Tell your Thinpost administrator if this doesn't clear up.</p>
                    </div>
                </div>
            </section>

            <section class="card" aria-labelledby="ops-worker-heading">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="ops-worker-heading">Sending workers</h2>
                        <HelpButton topic="sending workers">
                            <p>Sending workers (Thinpost Edge) are the servers that deliver your mail to recipients' mail servers. The only signal here is whether each worker keeps checking in. That doesn't prove it can reach the internet's mail servers.</p>
                            <p>Connected means the worker's connection hasn't expired. A connection lasts a short time and is renewed while the worker is healthy. Generation is an internal counter that identifies the worker's current connection.</p>
                        </HelpButton>
                    </div>
                </div>
                <div v-if="!ops.workers.length" class="card-body muted" data-testid="no-worker">No sending worker has connected.</div>
                <div v-else class="table-wrap">
                    <table class="data compact">
                        <thead>
                            <tr><th scope="col">Worker</th><th scope="col">Managed by</th><th scope="col" class="num">Generation</th><th scope="col">Status</th><th scope="col">Connection</th></tr>
                        </thead>
                        <tbody>
                            <tr v-for="w in ops.workers" :key="w.worker_id">
                                <th scope="row" class="nowrap"><code>{{ w.worker_id }}</code></th>
                                <td class="nowrap"><code>{{ w.controller_id }}</code></td>
                                <td class="num">{{ w.epoch }}</td>
                                <td class="nowrap">
                                    <span class="tag" :class="workerLive(w) ? 'success' : 'danger'">{{ workerLive(w) ? 'Connected' : 'Disconnected' }}</span>
                                </td>
                                <td class="nowrap" :title="`Expires ${formatDateTime(w.expires_at)}`">
                                    <template v-if="formatRemaining(w.expires_at, now)">valid for {{ formatRemaining(w.expires_at, now) }}</template>
                                    <template v-else>expired {{ formatAge(w.expires_at, now) }} ago</template>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </section>

            <section class="card" aria-labelledby="ops-queues-heading">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="ops-queues-heading">Queues</h2>
                        <HelpButton topic="queues">
                            <ul>
                                <li><strong>Due</strong>: ready to go now. <strong>Waiting</strong>: scheduled for later, such as a retry after a delay.</li>
                                <li><strong>Active</strong>: messages being prepared, deliveries being sent, webhook events being sent.</li>
                                <li><strong>Held</strong>: deliveries held by recipient rules or because tracking is unavailable. Someone needs to act on them.</li>
                                <li><strong>Failed</strong>: messages that couldn't be prepared for sending. <strong>Uncertain</strong>: deliveries whose result is unknown. <strong>Recovery</strong>: items held back after a data recovery.</li>
                                <li><strong>Oldest due</strong>: how long the oldest ready item has been waiting to be picked up. A growing number while a worker is connected deserves a look.</li>
                            </ul>
                        </HelpButton>
                    </div>
                </div>
                <div class="table-wrap">
                    <table class="data compact queues" data-testid="queues">
                        <thead>
                            <tr>
                                <th scope="col">Queue</th>
                                <th scope="col" class="num">Due</th>
                                <th scope="col" class="num">Waiting</th>
                                <th scope="col" class="num">Active</th>
                                <th scope="col" class="num">Uncertain</th>
                                <th scope="col" class="num">Held</th>
                                <th scope="col" class="num">Failed</th>
                                <th scope="col" class="num">Recovery</th>
                                <th scope="col" class="num">Oldest due</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="q in queueRows" :key="q.name" :data-queue="q.name">
                                <th scope="row" class="nowrap">{{ q.label }}</th>
                                <td v-for="(cell, i) in q.cells" :key="i" class="num" :class="{ zero: cell === 0, na: cell === null }" :title="q.titles[i]">
                                    <template v-if="cell === null">—</template>
                                    <template v-else>{{ formatCount(cell) }}</template>
                                </td>
                                <td class="num" :title="q.oldest ? `Due since ${formatDateTime(q.oldest)}` : undefined">
                                    <template v-if="q.oldest">{{ formatAge(q.oldest, now) }}</template>
                                    <template v-else>—</template>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </section>

            <section class="card" aria-labelledby="ops-inbound-heading">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="ops-inbound-heading">Inbound mail today</h2>
                        <HelpButton topic="inbound mail today">
                            <p>Mail received for all apps since 00:00 UTC. It counts messages, never addresses or content.</p>
                            <ul>
                                <li><strong>Received</strong>: messages accepted and handed to webhooks. <strong>Replies</strong> is how many of those answer an app's own message.</li>
                                <li><strong>Repeats</strong>: a sender tried again and Thinpost recognised the message, so it was not stored twice.</li>
                                <li><strong>Asked to retry</strong>: messages the sender had already sent, which Thinpost then asked it to send again later. Mail refused before the sender sent the message (for example while an app has no inbound webhook or is paused) isn't counted here.</li>
                            </ul>
                        </HelpButton>
                    </div>
                </div>
                <div class="card-body rows">
                    <p v-if="!ops.inbound" class="muted" data-testid="inbound-unavailable">Inbound counts aren't available from this version of Thinpost.</p>
                    <template v-else>
                        <dl class="facts" data-testid="inbound-today">
                            <div><dt>Received</dt><dd>{{ formatCount(ops.inbound.accepted) }}</dd></div>
                            <div><dt>Replies</dt><dd>{{ formatCount(ops.inbound.correlated) }}</dd></div>
                            <div><dt>Repeats</dt><dd>{{ formatCount(ops.inbound.duplicate) }}</dd></div>
                            <div><dt title="Sent in full, but nobody could receive it yet (for example an app with no inbound webhook, or a paused app)">Asked to retry: not ready</dt><dd>{{ formatCount(ops.inbound.tempfail_routing) }}</dd></div>
                            <div><dt title="Sent in full, but the app was over its daily or waiting limit">Asked to retry: over a limit</dt><dd>{{ formatCount(ops.inbound.tempfail_limit) }}</dd></div>
                            <div><dt title="Sent in full, but Thinpost had a problem storing it. Some problems can't be counted per app, so this can be low">Asked to retry: Thinpost problem</dt><dd>{{ formatCount(ops.inbound.tempfail_internal) }}</dd></div>
                        </dl>
                        <p class="hint">UTC day <time :datetime="ops.inbound.day">{{ ops.inbound.day }}</time>. The counts start again at midnight UTC.</p>
                    </template>
                </div>
            </section>

            <section class="card" aria-labelledby="ops-policy-heading" :aria-busy="isSubmitting">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="ops-policy-heading">Retry schedule</h2>
                        <HelpButton topic="the retry schedule">
                            <p>How Thinpost retries when a recipient's mail server temporarily can't take a message. It applies to every app. Thinpost tries up to the maximum number of attempts, waiting longer each time (with a little randomness) between the initial and maximum delay, and stops once the maximum age since the first attempt has passed.</p>
                            <p>Permanent rejections and uncertain results aren't retried automatically. A change only affects future scheduling. Retries already scheduled keep their times.</p>
                        </HelpButton>
                    </div>
                    <button type="button" class="btn sm primary" :disabled="!!policyBlocker || isSubmitting" :title="policyBlocker ?? undefined" @click="openPolicy">
                        <i class="fa-solid fa-pen" aria-hidden="true" />Edit
                    </button>
                </div>
                <div class="card-body rows">
                    <dl class="facts" data-testid="policy-facts">
                        <div><dt>Attempts</dt><dd>{{ formatCount(ops.retry_policy.max_attempts) }}</dd></div>
                        <div><dt>Initial delay</dt><dd>{{ formatMs(ops.retry_policy.initial_delay_ms) }}</dd></div>
                        <div><dt>Maximum delay</dt><dd>{{ formatMs(ops.retry_policy.max_delay_ms) }}</dd></div>
                        <div><dt>Maximum age</dt><dd>{{ formatMs(ops.retry_policy.max_age_ms) }}</dd></div>
                    </dl>
                    <p class="hint">
                        Change #{{ ops.retry_policy.revision }} · <time :datetime="ops.retry_policy.changed_at">{{ formatDateTime(ops.retry_policy.changed_at) }}</time>
                        <template v-if="ops.retry_policy.changed_by"> by <code>{{ ops.retry_policy.changed_by }}</code></template>
                    </p>
                    <p v-if="policyBlocker" class="hint">{{ policyBlocker }}</p>
                    <div v-if="isStale && !isPolicyOpen" class="notice warning" role="status">
                        <i class="fa-solid fa-circle-info" aria-hidden="true" />
                        <p class="notice-body">An earlier change may or may not have gone through, so the schedule shown may be out of date. Refresh to check it.</p>
                    </div>
                    <p v-if="savedNote" class="saved" role="status"><i class="fa-solid fa-check" aria-hidden="true" /> {{ savedNote }}</p>
                </div>
            </section>
        </template>

        <section class="card" aria-labelledby="ops-attention-heading">
            <div class="card-header">
                <div class="heading-with-help">
                    <h2 id="ops-attention-heading">Needs attention</h2>
                    <HelpButton topic="needs attention">
                        <p>Messages from all apps that need a look, oldest first. Open one to act on it: retry preparing it, retry a delivery held by recipient rules, or mark a delivery held for tracking as failed. These actions are recorded in the audit log.</p>
                        <p>Deliveries with an uncertain result can only be viewed. The recipient's mail server may have accepted the message, so Thinpost never resends them automatically, and there is no way to resolve them here.</p>
                    </HelpButton>
                </div>
            </div>
            <div v-if="attentionLoading && !attention" class="card-body"><Loader label="Loading the list" /></div>
            <div v-else-if="attentionError" class="card-body"><ApiErrorNotice :error="attentionError" :retry="loadAttention" /></div>
            <div v-else-if="attention && !attention.length" class="card-body muted" data-testid="attention-empty">Nothing needs attention.</div>
            <template v-else-if="attention">
                <div class="table-wrap">
                    <table class="data compact" data-testid="attention">
                        <thead>
                            <tr><th scope="col">App</th><th scope="col">Issue</th><th scope="col">Message</th><th scope="col">Submitted</th></tr>
                        </thead>
                        <tbody>
                            <tr v-for="item in attention" :key="`${item.message_id}/${item.delivery_id ?? ''}`">
                                <td class="nowrap"><span class="trunc" :title="item.app_name">{{ item.app_name }}</span></td>
                                <td class="nowrap"><span class="tag" :class="attentionTag(item.kind)">{{ ATTENTION_LABELS[item.kind].label }}</span></td>
                                <td class="nowrap">
                                    <RouterLink :to="{ name: 'app-message', params: { appId: item.app_id, messageId: item.message_id } }" :title="item.message_id">
                                        <code>{{ shortId(item.message_id, 8) }}…</code>
                                    </RouterLink>
                                    <code v-if="item.delivery_id" class="muted" :title="`Delivery ${item.delivery_id}`"> · {{ shortId(item.delivery_id, 6) }}…</code>
                                </td>
                                <td class="nowrap"><time :datetime="item.since">{{ formatDateTime(item.since) }}</time></td>
                            </tr>
                        </tbody>
                    </table>
                </div>
                <div v-if="moreError" class="card-body"><ApiErrorNotice :error="moreError" :retry="loadMore" /></div>
                <div class="card-body pager">
                    <span class="muted" role="status">{{ attention.length }} loaded{{ nextCursor ? '' : ' · end' }}</span>
                    <button v-if="nextCursor" type="button" class="btn sm" :disabled="isLoadingMore" @click="loadMore">{{ isLoadingMore ? 'Loading…' : 'Load more' }}</button>
                </div>
            </template>
        </section>

        <FeedbackLoopsCard :gate-mode="ops?.gate.mode ?? null" />

        <SendingIpsCard :gate-mode="ops?.gate.mode ?? null" />

        <section class="card" aria-labelledby="ops-runtime-heading">
            <div class="card-header">
                <div class="heading-with-help">
                    <h2 id="ops-runtime-heading">Enabled features</h2>
                    <HelpButton topic="enabled features">
                        <p>Features turned on for this Thinpost installation. For example, open and click tracking only works while Tracking is on. Your Thinpost administrator controls these.</p>
                    </HelpButton>
                </div>
            </div>
            <div class="card-body rows">
                <p v-if="runtimeError" class="muted" data-testid="runtime-unavailable">Unavailable: {{ runtimeError.message }}</p>
                <Loader v-else-if="!runtime" label="Loading enabled features" />
                <p v-else class="line" data-testid="runtime-features">
                    <span v-for="f in runtimeFeatures" :key="f.label" class="tag" :class="f.on ? 'success' : ''">{{ f.label }}: {{ f.on ? 'on' : 'off' }}</span>
                </p>
            </div>
        </section>

        <RetryPolicyModal
            v-if="isPolicyOpen && editPolicy"
            :current="editPolicy"
            :is-submitting="isSubmitting"
            :error="submitError"
            :blocker="policyBlocker"
            @close="closePolicy"
            @submit="savePolicy"
        />
    </div>
</template>

<script setup lang="ts">
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, onMounted, ref, shallowRef } from 'vue';
import { onBeforeRouteLeave } from 'vue-router';

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { authState, registerLeaveGuard } from '@/auth';
import { formatDateTime, shortId } from '@/format';
import {
    ATTENTION_LABELS,
    formatAge,
    formatMs,
    formatRemaining,
    isStaleDeployClosure,
    parseAttentionPage,
    parseOperations,
    parseRetryPolicy,
    type PolicyFields
} from '@/operations';
import { DefaultApi, type AttentionPage, type Operations, type RetryPolicy, type RuntimeInfo } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import FeedbackLoopsCard from '@/shared/components/feedback-loops-card.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import RetryPolicyModal from '@/shared/components/retry-policy-modal.vue';
import SendingIpsCard from '@/shared/components/sending-ips-card.vue';
import { formatCount } from '@/submission-rate';
import { parseRuntimeInfo } from '@/tracking';

type Item = AttentionPage['items'][number];

const REFRESH_MS = 30_000;
/** Statuses after which the server definitely did not apply the change. */
const DEFINITIVE_REFUSALS = new Set([400, 401, 403, 404, 409]);

const ops = shallowRef<Operations | null>(null);
const isLoading = ref(false);
const error = ref<ApiErrorDescription | null>(null);
const now = ref(Date.now());

const attention = shallowRef<Item[] | null>(null);
const nextCursor = ref<string | null>(null);
const attentionLoading = ref(false);
const attentionError = ref<ApiErrorDescription | null>(null);
const isLoadingMore = ref(false);
const moreError = ref<ApiErrorDescription | null>(null);

const runtime = shallowRef<RuntimeInfo | null>(null);
const runtimeError = ref<ApiErrorDescription | null>(null);

const isPolicyOpen = ref(false);
/** The policy the editor was seeded from; its revision is what Save checks, whatever reloads happen behind it. */
const editPolicy = shallowRef<RetryPolicy | null>(null);
/** Server clock minus browser clock at the last snapshot, so liveness and ages do not depend on the laptop clock. */
let clockOffset = 0;
const serverNow = () => Date.now() + clockOffset;
let pagesLoaded = 0;
const isSubmitting = ref(false);
const submitError = ref<ApiErrorDescription | null>(null);
const savedNote = ref<string | null>(null);
const isStale = ref(false);

let seq = 0;
let attentionSeq = 0;
let isUnmounted = false;
let refreshedAt = 0;
let refreshTimer: ReturnType<typeof setInterval> | undefined;
let clockTimer: ReturnType<typeof setInterval> | undefined;

const gateTag = computed(() => {
    switch (ops.value?.gate.mode) {
        case 'open':
            return { label: 'Open', cls: 'success' };
        case 'recovery':
            return { label: 'Recovery', cls: 'danger' };
        default:
            return { label: 'Closed', cls: 'warning' };
    }
});
const staleClosure = computed(() => !!ops.value && isStaleDeployClosure(ops.value.gate, now.value));
const policyBlocker = computed(() => {
    if (authState.status !== 'signed-in') return 'Sign in again to change the retry schedule.';
    if (ops.value && ops.value.gate.mode !== 'open') return 'Editing is only possible while mail intake is open.';
    return null;
});
const runtimeFeatures = computed(() => {
    const f = runtime.value?.features;
    return f
        ? [
              { label: 'Sending API', on: f.submission },
              { label: 'Tracking', on: f.tracking }
          ]
        : [];
});

const workerLive = (w: Operations['workers'][number]) => w.live && new Date(w.expires_at).getTime() > now.value;
const attentionTag = (kind: Item['kind']) => ATTENTION_LABELS[kind].tone;

/** Cells: Due, Waiting, Active, Uncertain, Held, Failed, Recovery; null where the queue has no such state. */
const queueRows = computed(() => {
    const o = ops.value;
    if (!o) return [];
    const { messages: m, deliveries: d, webhooks: w } = o;
    const heldTitle = `Recipient rules ${d.recipient_policy_held}, tracking ${d.tracking_held}`;
    const none = ['', '', '', '', '', '', ''];
    return [
        {
            name: 'messages',
            label: 'Messages',
            cells: [m.queued_due, m.queued_waiting, m.preparing, null, m.on_hold ?? null, m.preparation_failed, m.recovery_hold],
            titles: ['Waiting to be prepared, ready now', 'Waiting to be prepared, scheduled for later', 'Being prepared', '', 'On hold by their app until it releases or cancels them', 'Preparation failed', 'Held after data recovery'],
            oldest: m.oldest_due_at ?? null
        },
        {
            name: 'deliveries',
            label: 'Deliveries',
            cells: [d.queued_due, d.queued_waiting, d.in_flight, d.uncertain, d.recipient_policy_held + d.tracking_held, null, d.recovery_hold],
            titles: ['Queued, ready now', 'Queued, waiting to retry', 'Being sent', 'Outcome unknown', heldTitle, '', 'Held after data recovery'],
            oldest: d.oldest_due_at ?? null
        },
        {
            name: 'webhooks',
            label: 'Webhooks',
            cells: [w.queued_due, w.queued_waiting, w.in_flight, null, null, null, w.recovery_hold],
            titles: none,
            oldest: w.oldest_due_at ?? null
        }
    ];
});

async function load() {
    const mine = ++seq;
    isLoading.value = true;
    try {
        const { data } = await DefaultApi.getOperations();
        if (isUnmounted || mine !== seq) return;
        ops.value = parseOperations(data);
        error.value = null;
        clockOffset = Date.parse(ops.value.generated_at) - Date.now();
        now.value = serverNow();
        refreshedAt = Date.now();
    } catch (err) {
        if (isUnmounted || mine !== seq) return;
        // Never keep showing an old snapshot as current.
        ops.value = null;
        // The editor hides with the snapshot; keep its state consistent.
        if (!isSubmitting.value) isPolicyOpen.value = false;
        error.value = describeApiError(err, { unavailable: 'Thinpost couldn\'t read its data right now. Try again in a moment.' });
    } finally {
        if (mine === seq) isLoading.value = false;
    }
}

async function loadAttention() {
    const mine = ++attentionSeq;
    attentionLoading.value = true;
    attentionError.value = null;
    moreError.value = null;
    isLoadingMore.value = false;
    pagesLoaded = 1;
    try {
        const { data } = await DefaultApi.listOperationsAttention({ query: { limit: 50 } });
        if (isUnmounted || mine !== attentionSeq) return;
        const page = parseAttentionPage(data);
        attention.value = page.items;
        nextCursor.value = page.next_cursor ?? null;
    } catch (err) {
        if (isUnmounted || mine !== attentionSeq) return;
        attention.value = null;
        attentionError.value = describeApiError(err, { unavailable: 'The list is temporarily unavailable. Try again in a moment.' });
    } finally {
        if (mine === attentionSeq) attentionLoading.value = false;
    }
}

async function loadMore() {
    if (!nextCursor.value || isLoadingMore.value || !attention.value) return;
    const mine = attentionSeq;
    isLoadingMore.value = true;
    moreError.value = null;
    try {
        const { data } = await DefaultApi.listOperationsAttention({ query: { limit: 50, cursor: nextCursor.value } });
        if (isUnmounted || mine !== attentionSeq) return;
        const page = parseAttentionPage(data);
        attention.value = [...attention.value, ...page.items];
        nextCursor.value = page.next_cursor ?? null;
        pagesLoaded++;
    } catch (err) {
        if (isUnmounted || mine !== attentionSeq) return;
        moreError.value = describeApiError(err);
    } finally {
        if (mine === attentionSeq) isLoadingMore.value = false;
    }
}

async function loadRuntime() {
    try {
        const { data } = await DefaultApi.getRuntime();
        if (isUnmounted) return;
        runtime.value = parseRuntimeInfo(data);
        runtimeError.value = null;
    } catch (err) {
        if (isUnmounted) return;
        runtime.value = null;
        runtimeError.value = describeApiError(err);
    }
}

/** An automatic refresh keeps a list the user has paged into; a manual one starts over. */
function refresh(auto = false) {
    savedNote.value = null;
    void load();
    if (!(auto && pagesLoaded > 1)) void loadAttention();
}

function openPolicy() {
    if (policyBlocker.value || isSubmitting.value || !ops.value) return;
    submitError.value = null;
    savedNote.value = null;
    editPolicy.value = ops.value.retry_policy;
    isPolicyOpen.value = true;
}

function closePolicy() {
    if (isSubmitting.value) return;
    isPolicyOpen.value = false;
}

function summarize(p: RetryPolicy) {
    return `${formatCount(p.max_attempts)} attempts, ${formatMs(p.initial_delay_ms)} to ${formatMs(p.max_delay_ms)}, up to ${formatMs(p.max_age_ms)}`;
}

async function savePolicy(fields: PolicyFields) {
    const current = editPolicy.value;
    if (isSubmitting.value || policyBlocker.value || !current) return;
    isSubmitting.value = true;
    submitError.value = null;
    savedNote.value = null;
    // A newer reload must not overwrite the saved result with an older read.
    seq++;
    isLoading.value = false;

    let saved: RetryPolicy | null = null;
    let outcome: 'succeeded' | 'refused' | 'unknown';
    try {
        const { data } = await DefaultApi.setRetryPolicy({ body: { ...fields, expected_revision: current.revision } });
        saved = parseRetryPolicy(data);
        outcome = 'succeeded';
    } catch (err) {
        const status = apiErrorStatus(err);
        const description = describeApiError(err, {
            mutation: true,
            conflict: 'Someone else changed the retry schedule first. Nothing was changed. Refresh to see the current schedule.',
            unavailable: 'Changing the retry schedule is unavailable right now. Mail intake may be closed. Try again in a moment.'
        });
        if (status !== undefined && DEFINITIVE_REFUSALS.has(status) && !(err instanceof InvalidResponseError)) {
            outcome = 'refused';
            submitError.value = status === 409 ? description : { ...description, message: `${description.message} Nothing was changed.` };
        } else {
            outcome = 'unknown';
            isStale.value = true;
            submitError.value = { ...description, message: `${description.message} Refresh to check the current schedule before saving again.` };
        }
    } finally {
        isSubmitting.value = false;
    }

    if (isUnmounted) {
        showToast({
            message:
                outcome === 'succeeded'
                    ? `The retry schedule was saved after you left the page: ${summarize(saved!)}.`
                    : outcome === 'unknown'
                      ? 'The retry schedule change may or may not have gone through. Open Operations to check it.'
                      : 'The retry schedule change was refused. Nothing was changed.',
            durationSecs: 10
        });
        return;
    }
    if (outcome !== 'succeeded' || !saved) {
        if (submitError.value?.status === 409) {
            // Close the editor so a resubmit cannot overwrite the newer policy; the reload shows it.
            isPolicyOpen.value = false;
            savedNote.value = 'Someone else changed the retry schedule. Nothing was changed; the current schedule is shown.';
            void load();
        }
        return;
    }
    if (ops.value) ops.value = { ...ops.value, retry_policy: saved };
    isStale.value = false;
    isPolicyOpen.value = false;
    savedNote.value = `Saved: ${summarize(saved)}.`;
    showToast({ message: 'Retry policy saved.', durationSecs: 5 });
}

async function confirmLeave() {
    if (!isSubmitting.value) return true;
    return showConfirm(
        'Leave while the retry schedule is being saved?',
        'The request has been sent and may still take effect. This page will not show the result; open Operations afterwards to check the policy.'
    );
}

onBeforeRouteLeave(confirmLeave);
const unregisterLeaveGuard = registerLeaveGuard(confirmLeave);

function onVisible() {
    if (document.visibilityState === 'visible' && !isSubmitting.value && !isPolicyOpen.value && Date.now() - refreshedAt >= REFRESH_MS) refresh(true);
}

onMounted(() => {
    void load();
    void loadAttention();
    void loadRuntime();
    // Refresh while the tab is visible; a hidden tab catches up when it is shown again.
    refreshTimer = setInterval(() => {
        if (document.visibilityState === 'visible' && !isSubmitting.value && !isPolicyOpen.value) refresh(true);
    }, REFRESH_MS);
    clockTimer = setInterval(() => {
        if (document.visibilityState === 'visible') now.value = serverNow();
    }, 1000);
    document.addEventListener('visibilitychange', onVisible);
});

onBeforeUnmount(() => {
    isUnmounted = true;
    clearInterval(refreshTimer);
    clearInterval(clockTimer);
    document.removeEventListener('visibilitychange', onVisible);
    unregisterLeaveGuard();
});
</script>

<style lang="scss" scoped>
#operations {
    display: flex;
    flex-direction: column;
    gap: 16px;
    min-width: 0;
}

.header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
}

.header-actions {
    display: flex;
    align-items: center;
    gap: 10px;
}

.asof {
    font-size: 12px;
    white-space: nowrap;
}

.card-header h2 {
    display: flex;
    align-items: center;
    gap: 8px;
}

.rows {
    display: flex;
    flex-direction: column;
    gap: 10px;
    font-size: 13px;
}

.line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 4px 8px;
}

.facts dd {
    white-space: nowrap;
}

.nowrap {
    white-space: nowrap;
}

.compact :is(th, td) {
    padding-inline: 8px;
}

.queues .zero,
.queues .na {
    color: var(--text-3);
}

.num {
    text-align: right;
    font-variant-numeric: tabular-nums;
}

.trunc {
    display: inline-block;
    max-width: 220px;
    overflow: hidden;
    text-overflow: ellipsis;
    vertical-align: bottom;
    white-space: nowrap;
}

code {
    font-size: 12px;
}

.pager {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
}

.saved {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--success);
}
</style>
