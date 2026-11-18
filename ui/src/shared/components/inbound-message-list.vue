<template>
    <section class="card" aria-labelledby="inbound-messages-heading" :aria-busy="isLoading || isRefreshing">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 id="inbound-messages-heading">Received messages</h2>
                <HelpButton topic="received messages">
                    <p>Mail sent to this application's receiving domains, newest first. Each message is handed to your inbound webhook; open one to see who sent it, whether it passed the sender checks and what it said.</p>
                    <ul>
                        <li><strong>Reply</strong> means the message quotes one of your app's own messages. Anyone who saw that message could do the same, so it isn't proof of who wrote it.</li>
                        <li><strong>DMARC</strong> is the result of the sender's own checks (SPF and DKIM). Thinpost reports it and never rejects mail because of it.</li>
                        <li>Messages hide their subject when they reply to a message sent with Enhanced Protection. Opening their content is recorded in the audit log.</li>
                        <li>Content is kept for 30 days. After that the message stays in this list, marked <strong>Expired</strong>, but its content can't be shown or sent again.</li>
                    </ul>
                </HelpButton>
            </div>
            <button type="button" class="btn sm" :disabled="isLoading || isLoadingMore || isRefreshing" @click="refresh">
                <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isRefreshing }" aria-hidden="true" />
                {{ isRefreshing ? 'Refreshing…' : 'Refresh' }}
            </button>
        </div>

        <div v-if="isLoading" class="card-body"><Loader label="Loading received messages" /></div>

        <div v-else-if="loadError" class="card-body"><ApiErrorNotice :error="loadError" :retry="load" /></div>

        <div v-else-if="messages && !messages.length" class="card-body empty" data-testid="no-inbound-messages">
            <i class="fa-solid fa-inbox" aria-hidden="true" />
            <h2>No messages received yet</h2>
            <p class="hint">Mail sent to a verified receiving domain of this application shows up here.</p>
        </div>

        <template v-else-if="messages">
            <div v-if="refreshError" class="card-body"><ApiErrorNotice :error="refreshError" :retry="refresh" /></div>
            <div class="table-wrap" :aria-busy="isRefreshing">
                <table class="data inbound-table" data-testid="inbound-messages">
                    <caption class="sr-only">Messages received by {{ appName }}, newest first</caption>
                    <thead>
                        <tr>
                            <th scope="col">Received</th>
                            <th scope="col">From</th>
                            <th scope="col">To</th>
                            <th scope="col">Subject</th>
                            <th scope="col" class="num">Size</th>
                            <th scope="col" title="The sender's own SPF and DKIM checks, as DMARC reports them">DMARC</th>
                            <th scope="col">Webhook</th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr v-for="row in messages" :key="row.id" :data-inbound="row.id">
                            <th scope="row" class="when">
                                <RouterLink :to="{ name: 'app-inbound-message', params: { appId, inboundId: row.id } }" :title="row.id">
                                    <time :datetime="row.receivedAt">{{ formatDateTime(row.receivedAt) }}</time>
                                    <span class="sr-only"> message {{ row.id }}</span>
                                </RouterLink>
                                <span v-if="row.contentExpired" class="tag" title="Content is kept for 30 days and has been removed. The message can't be shown or sent again.">Expired</span>
                            </th>
                            <td class="from">
                                <span v-if="row.from" class="ellipsis" :title="row.from">{{ row.from }}</span>
                                <span v-else class="muted">Not recorded</span>
                            </td>
                            <td class="to">
                                <span v-if="row.recipient" class="ellipsis" :title="row.recipient">{{ row.recipient }}</span>
                                <span v-else class="muted">Not recorded</span>
                                <span v-if="row.recipientCount > 1" class="tag" :title="`${row.recipientCount} recipients`">+{{ row.recipientCount - 1 }}</span>
                            </td>
                            <td class="subject">
                                <span v-if="row.enhancedProtection" class="muted" title="Protected subject. Open the message to view it; that is recorded in the audit log.">
                                    <i class="fa-solid fa-lock" aria-hidden="true" /> Protected<span class="sr-only"> subject</span>
                                </span>
                                <span v-else-if="row.subject" class="ellipsis" :title="row.subject">{{ row.subject }}</span>
                                <span v-else class="muted">(no subject)</span>
                            </td>
                            <td class="num size">{{ formatBytes(row.sizeBytes) }}</td>
                            <td class="auth">
                                <span class="tag" :class="authResult(row.dmarc).tone" :title="authResult(row.dmarc).description">{{ authResult(row.dmarc).label }}</span>
                                <span v-if="row.correlated" class="tag info" title="Quotes one of this app's own messages. That is a claim in the message's headers, not proof of who wrote it.">Reply</span>
                            </td>
                            <td class="hook">
                                <span class="tag" :class="WEBHOOK_SUMMARY[row.webhook.status].tone" :title="WEBHOOK_SUMMARY[row.webhook.status].description">
                                    {{ WEBHOOK_SUMMARY[row.webhook.status].label }}
                                </span>
                            </td>
                        </tr>
                    </tbody>
                </table>
            </div>

            <div v-if="moreError" class="card-body"><ApiErrorNotice :error="moreError" :retry="loadMore" /></div>

            <div class="card-body pager">
                <span class="muted" role="status">{{ messages.length }} {{ messages.length === 1 ? 'message' : 'messages' }} loaded{{ nextCursor ? '' : ' · end of history' }}</span>
                <button v-if="nextCursor" type="button" class="btn" :disabled="isLoadingMore || isRefreshing" @click="loadMore">
                    {{ isLoadingMore ? 'Loading…' : 'Load older messages' }}
                </button>
            </div>
        </template>
    </section>
</template>

<script lang="ts" setup>
import { onBeforeUnmount, ref, shallowRef, watch } from 'vue';

import { describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { formatBytes, formatDateTime } from '@/format';
import { authResult, readInboundSummary, WEBHOOK_SUMMARY, type InboundRow } from '@/inbound-messages';
import { DefaultApi } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';

const props = defineProps<{ appId: string; appName: string }>();

const PAGE_SIZE = 50;
const MAX_CURSOR_LENGTH = 512;
const ERRORS = {
    notFound: 'The application was not found.',
    unavailable: 'Received messages are unavailable right now. Try again in a moment.'
};

const messages = shallowRef<InboundRow[] | null>(null);
const nextCursor = ref<string | null>(null);
const isLoading = ref(true);
const loadError = ref<ApiErrorDescription | null>(null);
const isLoadingMore = ref(false);
const moreError = ref<ApiErrorDescription | null>(null);
const isRefreshing = ref(false);
const refreshError = ref<ApiErrorDescription | null>(null);

let loadSeq = 0;
let isUnmounted = false;
onBeforeUnmount(() => {
    isUnmounted = true;
});

watch(() => props.appId, load, { immediate: true });

interface Page {
    rows: InboundRow[];
    next: string | null;
}

async function fetchPage(cursor?: string): Promise<Page> {
    const { data } = await DefaultApi.listInboundMessages({ path: { appId: props.appId }, query: cursor ? { limit: PAGE_SIZE, cursor } : { limit: PAGE_SIZE } });
    if (!Array.isArray(data?.messages)) throw new InvalidResponseError();
    const rows: InboundRow[] = [];
    for (const raw of data.messages) {
        const row = readInboundSummary(raw);
        if (!row) throw new InvalidResponseError();
        rows.push(row);
    }
    const next = data.next_cursor;
    if (next !== undefined && (typeof next !== 'string' || next.length > MAX_CURSOR_LENGTH)) throw new InvalidResponseError();
    return { rows, next: next || null };
}

async function load() {
    const seq = ++loadSeq;
    isLoading.value = true;
    isRefreshing.value = false;
    isLoadingMore.value = false;
    loadError.value = null;
    moreError.value = null;
    refreshError.value = null;
    messages.value = null;
    nextCursor.value = null;
    try {
        const page = await fetchPage();
        if (seq !== loadSeq || isUnmounted) return;
        messages.value = page.rows;
        nextCursor.value = page.next;
    } catch (err) {
        if (seq !== loadSeq || isUnmounted) return;
        loadError.value = describeApiError(err, ERRORS);
    } finally {
        if (seq === loadSeq) isLoading.value = false;
    }
}

/** Reloads the first page while keeping the rows on screen. */
async function refresh() {
    if (isRefreshing.value) return;
    if (messages.value === null) return load();
    // Invalidates any older page in flight; its rows would belong to the previous listing.
    const seq = ++loadSeq;
    isRefreshing.value = true;
    isLoadingMore.value = false;
    refreshError.value = null;
    moreError.value = null;
    try {
        const page = await fetchPage();
        if (seq !== loadSeq || isUnmounted) return;
        messages.value = page.rows;
        nextCursor.value = page.next;
    } catch (err) {
        if (seq === loadSeq && !isUnmounted) {
            const description = describeApiError(err, ERRORS);
            refreshError.value = { ...description, message: `${description.message} The rows below are from the last time they loaded.` };
        }
    } finally {
        if (seq === loadSeq) isRefreshing.value = false;
    }
}

async function loadMore() {
    const cursor = nextCursor.value;
    if (!cursor || isLoadingMore.value) return;
    const seq = loadSeq;
    isLoadingMore.value = true;
    moreError.value = null;
    try {
        const page = await fetchPage(cursor);
        if (seq !== loadSeq || isUnmounted) return;
        const seen = new Set((messages.value ?? []).map(m => m.id));
        messages.value = [...(messages.value ?? []), ...page.rows.filter(m => !seen.has(m.id))];
        nextCursor.value = page.next;
    } catch (err) {
        // Keep the loaded rows and the cursor so the same page can be retried.
        if (seq === loadSeq && !isUnmounted) moreError.value = describeApiError(err, ERRORS);
    } finally {
        if (seq === loadSeq) isLoadingMore.value = false;
    }
}
</script>

<style lang="scss" scoped>
.when {
    white-space: nowrap;

    .tag {
        margin-left: 6px;
    }
}

.inbound-table :is(th, td) {
    padding-inline: 8px;
}

.from,
.to,
.subject,
.auth,
.hook {
    white-space: nowrap;
}

.ellipsis {
    display: inline-block;
    max-width: 190px;
    overflow: hidden;
    text-overflow: ellipsis;
    vertical-align: middle;
    white-space: nowrap;
}

.to .tag,
.auth .tag + .tag {
    margin-left: 6px;
}

.pager {
    padding-top: 12px;
}
</style>
