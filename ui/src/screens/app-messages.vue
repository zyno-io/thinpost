<template>
    <div id="app-messages">
        <section class="card" aria-labelledby="messages-heading">
            <div class="card-header">
                <div class="heading-with-help">
                    <h2 id="messages-heading">
                        Messages
                    </h2>
                    <HelpButton topic="message search">
                        <p>Newest first. {{ FIELD_COPY[draft.field].label }}: {{ FIELD_COPY[draft.field].hint }}</p>
                        <p v-if="excludesProtected(draft.field)" id="ep-search-note">
                            {{ draft.field === 'any' ? PROTECTED_SEARCH_NOTE_ANY : PROTECTED_SEARCH_NOTE }}
                        </p>
                        <p>
                            Enhanced Protection is chosen for each message when it is sent. Those messages hide their subject here. You can reveal it,
                            and each reveal is recorded in the audit log.
                        </p>
                    </HelpButton>
                </div>
                <div class="header-actions">
                    <button
                        type="button"
                        class="btn sm"
                        :disabled="!!testBlocker || testSend.isSubmitting.value"
                        :aria-describedby="testBlocker ? 'test-send-hint' : undefined"
                        @click="testSend.open()"
                    >
                        <i class="fa-solid fa-paper-plane" aria-hidden="true" />
                        Send test message
                    </button>
                    <button type="button" class="btn sm" :disabled="isLoading || isSearching || isLoadingMore || isRefreshing" @click="refresh">
                        <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isRefreshing }" aria-hidden="true" />
                        {{ isRefreshing ? 'Refreshing…' : 'Refresh' }}
                    </button>
                </div>
            </div>

            <div class="card-body messages-body">
                <form ref="formEl" class="filter-bar" role="search" aria-label="Search messages" @submit.prevent="submitSearch">
                    <select class="select" aria-label="Search field" :value="draft.field" @change="onFieldChange">
                        <option v-for="field in SEARCH_FIELDS" :key="field" :value="field">{{ FIELD_COPY[field].label }}</option>
                    </select>
                    <div class="search-input">
                        <i class="fa-solid fa-magnifying-glass" aria-hidden="true" />
                        <input
                            ref="inputEl"
                            class="input"
                            type="search"
                            aria-label="Search messages"
                            autocomplete="off"
                            :spellcheck="false"
                            :maxlength="SEARCH_MAX_LENGTH"
                            :placeholder="FIELD_COPY[draft.field].placeholder"
                            :value="draft.q"
                            @input="onQueryInput"
                        />
                        <button v-if="draft.q" type="button" class="btn ghost sm icon" aria-label="Clear search" @click="clearSearch">
                            <i class="fa-solid fa-xmark" aria-hidden="true" />
                        </button>
                    </div>
                    <span class="muted search-status" role="status">{{ searchStatus }}</span>
                </form>

                <p v-if="testBlocker" id="test-send-hint" class="hint">{{ testBlocker }}</p>
                <ApiErrorNotice v-if="testSend.outcomeError.value" :error="testSend.outcomeError.value" />
                <p v-if="validationHint" class="hint validation" role="status">{{ validationHint }}</p>
                <p v-if="isSearchActive && excludesProtected(active.field)" class="hint ep-excluded">Excludes Enhanced Protection subjects.</p>

                <ApiErrorNotice v-if="refreshError" :error="refreshError" :retry="refresh" />
                <ApiErrorNotice v-if="revealError" :error="revealError" />
            </div>

            <div v-if="isLoading" class="card-body"><Loader label="Loading messages" /></div>

            <div v-else-if="loadError" class="card-body"><ApiErrorNotice :error="loadError" :retry="load" /></div>

            <div v-else-if="messages && !messages.length" class="card-body empty">
                <i class="fa-solid fa-envelope" aria-hidden="true" />
                <template v-if="isSearchActive">
                    <h2>No matches</h2>
                    <p v-if="excludesProtected(active.field)" class="hint">Excludes Enhanced Protection subjects.</p>
                </template>
                <template v-else>
                    <h2>No messages yet</h2>
                </template>
            </div>

            <template v-else-if="messages">
                <div class="table-wrap" :class="{ 'is-stale': isSearching || isRefreshing }" :aria-busy="isSearching || isRefreshing">
                    <table class="data messages-table">
                        <caption class="sr-only">Messages for {{ app.name }}, newest first</caption>
                        <thead>
                            <tr>
                                <th scope="col" title="When your app submitted the message to Thinpost">Submitted</th>
                                <th scope="col">Message</th>
                                <th scope="col">Sender domain</th>
                                <th scope="col">To</th>
                                <th scope="col">Subject</th>
                                <th scope="col">Status</th>
                                <th scope="col" class="num" title="Recipients whose mail server accepted the message / total recipients">Recipients</th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="row in messages" :key="row.id">
                                <td class="when">
                                    <time :datetime="row.acceptedAt">{{ formatDateTime(row.acceptedAt) }}</time>
                                </td>
                                <th scope="row" class="id">
                                    <RouterLink
                                        :to="{ name: 'app-message', params: { appId: app.id, messageId: row.id } }"
                                        :title="row.id"
                                        :aria-label="`Message ${row.id}`"
                                    >
                                        <code>{{ shortId(row.id, 8) }}…</code>
                                    </RouterLink>
                                </th>
                                <td>
                                    <button
                                        v-if="row.senderDomain"
                                        class="link-btn domain-filter sender"
                                        :title="row.senderDomain"
                                        type="button"
                                        :aria-label="`Search messages from sender domain ${row.senderDomain}`"
                                        @click="applySearch('sender_domain', row.senderDomain)"
                                    >
                                        {{ row.senderDomain }}
                                    </button>
                                    <span v-else class="muted">Not recorded</span>
                                </td>
                                <td class="to">
                                    <template v-if="firstRecipient(row)">
                                        <span class="addr" :title="recipientTitle(row)">
                                            <button
                                                v-if="firstRecipient(row)!.domain"
                                                class="link-btn domain-filter local"
                                                type="button"
                                                :aria-label="`Search messages to ${firstRecipient(row)!.address}`"
                                                @click="applySearch('recipient', firstRecipient(row)!.address)"
                                            >
                                                {{ firstRecipient(row)!.local }}
                                            </button>
                                            <span v-else class="local">{{ firstRecipient(row)!.local }}</span>
                                            <template v-if="firstRecipient(row)!.domain">
                                                <span class="at">@</span>
                                                <button
                                                    class="link-btn domain-filter"
                                                    type="button"
                                                    :aria-label="`Search messages to recipient domain ${firstRecipient(row)!.domain}`"
                                                    @click="applySearch('recipient_domain', firstRecipient(row)!.domain!)"
                                                >
                                                    {{ firstRecipient(row)!.domain }}
                                                </button>
                                            </template>
                                        </span>
                                        <span v-if="extraRecipients(row) > 0" class="tag">+{{ extraRecipients(row) }}</span>
                                    </template>
                                    <span v-else class="muted">Not recorded</span>
                                </td>
                                <td class="subject">
                                    <template v-if="row.subject.kind === 'text'">
                                        <span v-if="row.subject.text" class="ellipsis" :title="row.subject.text">{{ row.subject.text }}</span>
                                        <span v-else class="muted">(no subject)</span>
                                    </template>
                                    <span v-else-if="row.subject.kind === 'not-recorded'" class="muted">Not recorded</span>
                                    <template v-else>
                                        <template v-if="revealedText(row.id) !== null">
                                            <span v-if="revealedText(row.id)" class="ellipsis" :title="revealedText(row.id)!">{{ revealedText(row.id) }}</span>
                                            <span v-else class="muted">(no subject)</span>
                                            <span class="tag info">Revealed</span>
                                            <button type="button" class="link-btn" @click="hideSubject(row.id)">
                                                Hide<span class="sr-only"> subject of message {{ row.id }}</span>
                                            </button>
                                        </template>
                                        <span v-else-if="revealedOf(row.id)?.kind === 'none'" class="muted">No subject recorded</span>
                                        <template v-else>
                                            <span class="protected-subject muted" title="Protected subject"><i class="fa-solid fa-lock" aria-hidden="true" /> Protected<span class="sr-only"> subject</span></span>
                                            <button class="btn ghost sm"
                                                type="button"
                                                :disabled="authState.status !== 'signed-in' || revealing.has(row.id)"
                                                @click="reveal(row)"
                                            >
                                                {{ revealing.has(row.id) ? 'Revealing…' : 'Reveal' }}<span class="sr-only"> subject of message {{ row.id }}</span>
                                            </button>
                                        </template>
                                    </template>
                                </td>
                                <td class="status">
                                    <span class="tag" :class="tagClass(messageStatus(row.status).tone)" :title="messageStatus(row.status).description">
                                        {{ messageStatus(row.status).label }}
                                    </span>
                                    <span v-if="row.operatorTest" class="tag info" data-testid="console-test-tag" title="Sent from the console as a test">Console test</span>
                                    <span v-if="row.operatorTest?.bypassLimits" class="tag warning" data-testid="limits-skipped-tag" title="The sending limits were skipped and not counted">
                                        limits skipped
                                    </span>
                                    <span v-if="row.operatorTest?.bypassRecipientPolicy" class="tag warning" data-testid="allowlist-skipped-tag" title="The recipient allowlist was skipped">
                                        allowlist skipped
                                    </span>
                                    <span v-if="row.smtp" class="tag info" data-testid="smtp-tag" :title="`Sent over SMTP with ${row.smtp.credentialName}`">SMTP</span>
                                    <span v-if="row.enhancedProtection" class="ep-icon" title="Enhanced Protection">
                                        <i class="fa-solid fa-shield-halved" aria-hidden="true" /><span class="sr-only">Enhanced Protection</span>
                                    </span>
                                </td>
                                <td class="num deliveries">
                                    {{ row.acceptedCount }}/{{ row.deliveryCount }}<span class="sr-only"> accepted</span>
                                    <span v-if="row.failedCount > 0" class="tag danger">{{ row.failedCount }} failed</span>
                                    <span v-if="row.uncertainCount > 0" class="tag warning">{{ row.uncertainCount }} uncertain</span>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>

                <div v-if="moreError" class="card-body"><ApiErrorNotice :error="moreError" :retry="loadMore" /></div>

                <div class="card-body pager">
                    <span class="muted" role="status"
                        >{{ messages.length }} {{ messages.length === 1 ? 'message' : 'messages' }} loaded{{
                            nextCursor ? '' : isSearchActive ? ' · end of results' : ' · end of history'
                        }}</span
                    >
                    <button v-if="nextCursor" type="button" class="btn" :disabled="isLoadingMore || isRefreshing || isSearching" @click="loadMore">
                        {{ isLoadingMore ? 'Loading…' : 'Load older messages' }}
                    </button>
                </div>
            </template>
        </section>

        <TestMessageModal
            v-if="testSend.isOpen.value"
            :domains="testSend.domains.value"
            :domains-error="testSend.domainsError.value"
            :preselect="testSend.preselect.value"
            :recipient-policy="testSend.recipientPolicy.value"
            :is-submitting="testSend.isSubmitting.value"
            :error="testSend.error.value"
            @close="testSend.close"
            @submit="testSend.submit"
        />
    </div>
</template>

<script lang="ts" setup>
import { computed, nextTick, onBeforeUnmount, reactive, ref, shallowRef, watch } from 'vue';
import { useRouter } from 'vue-router';

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { confirmProtectedAccess, withAuditSuffix } from '@/enhanced-protection';
import { formatDateTime, shortId } from '@/format';
import {
    excludesProtected,
    FIELD_COPY,
    normalizeMessageId,
    normalizeSearch,
    PROTECTED_SEARCH_NOTE,
    PROTECTED_SEARCH_NOTE_ANY,
    SEARCH_DEBOUNCE_MS,
    SEARCH_FIELDS,
    SEARCH_MAX_LENGTH,
    searchValidationHint,
    type MessageSearch,
    type SearchField
} from '@/message-search';
import { savedSearch, saveSearch } from '@/message-search-store';
import { readMessageSummary, type MessageRow } from '@/message-summary';
import { DefaultApi } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import TestMessageModal from '@/shared/components/test-message-modal.vue';
import { messageStatus, tagClass } from '@/states';
import { useTestSend } from '@/test-send';

const PAGE_SIZE = 50;
const MAX_CURSOR_LENGTH = 512;
const NOT_FOUND = 'The app was not found.';
const SEARCH_UNAVAILABLE = 'Search is temporarily unavailable. Try again in a moment.';

type Revealed = { kind: 'text'; text: string } | { kind: 'none' };

const router = useRouter();
const { app, appId } = useAppContext();

const formEl = ref<HTMLFormElement | null>(null);
const inputEl = ref<HTMLInputElement | null>(null);

// Search terms never leave memory: they are not put in the URL, router state or browser storage.
const initial = savedSearch(appId.value);
const draft = reactive<MessageSearch>(initial ? { ...initial } : { q: '', field: 'any' });
const active = shallowRef<MessageSearch>(normalizeSearch(draft));
const validationHint = ref<string | null>(null);

const messages = shallowRef<MessageRow[] | null>(null);
const nextCursor = ref<string | null>(null);
const isLoading = ref(true);
const isSearching = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const isLoadingMore = ref(false);
const moreError = ref<ApiErrorDescription | null>(null);
const isRefreshing = ref(false);
const refreshError = ref<ApiErrorDescription | null>(null);

const revealed = shallowRef(new Map<string, Revealed>());
const revealing = ref(new Set<string>());
const revealError = ref<ApiErrorDescription | null>(null);


// ---- Test message ----
const testSend = useTestSend({
    appId,
    isActive: computed(() => app.value.active),
    onUnknownOutcome: async () => {
        await refresh();
        return !refreshError.value && !loadError.value;
    }
});
const testBlocker = testSend.blocker;

const isSearchActive = computed(() => active.value.q !== '');
const searchStatus = computed(() => {
    if (isSearching.value || (isLoading.value && isSearchActive.value)) return 'Searching…';
    if (isSearchActive.value && messages.value && !loadError.value) return `${messages.value.length} matching ${messages.value.length === 1 ? 'message' : 'messages'} loaded`;
    return '';
});

let loadSeq = 0;
let controller: AbortController | null = null;
let timer: ReturnType<typeof setTimeout> | undefined;
let navigateOnResult = false;
let isUnmounted = false;

watch(
    [appId, () => active.value.q, () => (active.value.q ? active.value.field : '')],
    () => void load(),
    { immediate: true }
);
watch(
    () => authState.status,
    status => {
        if (status !== 'signed-in') clearRevealed();
    }
);
onBeforeUnmount(() => {
    isUnmounted = true;
    clearTimeout(timer);
    controller?.abort();
    clearRevealed();
});

// ---- Search input ----

function sameSearch(a: MessageSearch, b: MessageSearch): boolean {
    return a.q === b.q && a.field === b.field;
}

/** Applies the draft as the active search unless it cannot be sent. Returns whether it is sendable. */
function applyDraft(): boolean {
    const next = normalizeSearch(draft);
    const hint = searchValidationHint(next);
    validationHint.value = hint;
    if (hint) return false;
    saveSearch(appId.value, next);
    if (!sameSearch(next, active.value)) active.value = next;
    return true;
}

function onQueryInput(event: Event) {
    draft.q = (event.target as HTMLInputElement).value;
    clearTimeout(timer);
    timer = setTimeout(applyDraft, SEARCH_DEBOUNCE_MS);
}

function onFieldChange(event: Event) {
    draft.field = (event.target as HTMLSelectElement).value as SearchField;
    clearTimeout(timer);
    applyDraft();
}

function clearSearch() {
    clearTimeout(timer);
    draft.q = '';
    applyDraft();
    void nextTick(() => inputEl.value?.focus());
}

/** Explicit submit: runs now, and may jump to a message when the query is an exact ID. */
function submitSearch() {
    clearTimeout(timer);
    if (!applyDraft()) return;
    navigateOnResult = true;
    // An unchanged search does not retrigger the watcher; reload explicitly.
    const seqBefore = loadSeq;
    void nextTick(() => {
        if (seqBefore === loadSeq) void load();
    });
}

function applySearch(field: 'recipient' | 'sender_domain' | 'recipient_domain', value: string) {
    clearTimeout(timer);
    draft.q = value;
    draft.field = field;
    applyDraft();
    void nextTick(() => {
        formEl.value?.scrollIntoView?.({ block: 'nearest' });
        inputEl.value?.focus();
    });
}

function splitAddress(address: string): { address: string; local: string; domain: string | null } {
    const at = address.lastIndexOf('@');
    if (at <= 0 || at === address.length - 1) return { address, local: address, domain: null };
    return { address, local: address.slice(0, at), domain: address.slice(at + 1) };
}

function firstRecipient(row: MessageRow): { address: string; local: string; domain: string | null } | null {
    const first = toRecipients(row)[0];
    return first === undefined ? null : splitAddress(first);
}

/** The To addresses among the recorded ones; Cc and Bcc recipients are not part of the To column. */
function toRecipients(row: MessageRow): string[] {
    if (row.recipients.kind !== 'list') return [];
    const kinds = row.recipients.kinds;
    return row.recipients.shown.filter((_, i) => kinds[i] === 'to');
}

function recipientTitle(row: MessageRow): string {
    return toRecipients(row).join(', ');
}

function extraRecipients(row: MessageRow): number {
    if (row.recipients.kind !== 'list') return 0;
    // Recorded recipients are in To, Cc, Bcc order. When a Cc or Bcc address is among those shown, the
    // To addresses are exactly the To ones shown; otherwise the rest may be To, so the total is used.
    const shownTo = toRecipients(row).length;
    if (shownTo < row.recipients.shown.length) return Math.max(0, shownTo - 1);
    const total = row.recipients.total ?? row.recipients.shown.length;
    return Math.max(0, total - 1);
}

// ---- Loading ----

interface Page {
    messages: MessageRow[];
    nextCursor: string | null;
}

async function fetchPage(id: string, search: MessageSearch, signal: AbortSignal, cursor?: string): Promise<Page> {
    let response;
    if (search.q) {
        response = await DefaultApi.searchAppMessages({
            path: { appId: id },
            body: { q: search.q, field: search.field, limit: PAGE_SIZE, ...(cursor ? { cursor } : {}) },
            signal
        });
    } else {
        response = await DefaultApi.listAppMessages({ path: { appId: id }, query: cursor ? { limit: PAGE_SIZE, cursor } : { limit: PAGE_SIZE } });
    }
    const data = response.data;
    if (!Array.isArray(data?.messages)) throw new InvalidResponseError();
    const rows: MessageRow[] = [];
    for (const raw of data.messages) {
        const row = readMessageSummary(raw);
        if (!row) throw new InvalidResponseError();
        rows.push(row);
    }
    const next = data.next_cursor;
    if (next !== undefined && (typeof next !== 'string' || next.length > MAX_CURSOR_LENGTH)) throw new InvalidResponseError();
    return { messages: rows, nextCursor: next || null };
}

function describe(err: unknown, search: MessageSearch): ApiErrorDescription {
    return describeApiError(err, { notFound: NOT_FOUND, ...(search.q ? { unavailable: SEARCH_UNAVAILABLE } : {}) });
}

function beginRequest(): AbortSignal {
    controller?.abort();
    controller = new AbortController();
    return controller.signal;
}

async function load() {
    const id = appId.value;
    const search = active.value;
    const seq = ++loadSeq;
    const signal = beginRequest();
    const submitted = navigateOnResult;
    navigateOnResult = false;
    if (messages.value) isSearching.value = true;
    else isLoading.value = true;
    loadError.value = null;
    moreError.value = null;
    isLoadingMore.value = false;
    refreshError.value = null;
    isRefreshing.value = false;
    clearRevealed();

    try {
        const page = await fetchPage(id, search, signal);
        if (seq !== loadSeq || isUnmounted) return;
        messages.value = page.messages;
        nextCursor.value = page.nextCursor;
        if (submitted) jumpToExactMatch(id, search, page.messages);
    } catch (err) {
        if (seq !== loadSeq || isUnmounted) return;
        messages.value = null;
        nextCursor.value = null;
        if (search.q && apiErrorStatus(err) === 400) {
            validationHint.value = `This text has nothing searchable for ${FIELD_COPY[search.field].label}. ${FIELD_COPY[search.field].hint}`;
        } else {
            loadError.value = describe(err, search);
        }
    } finally {
        if (seq === loadSeq) {
            isLoading.value = false;
            isSearching.value = false;
        }
    }
}

function jumpToExactMatch(id: string, search: MessageSearch, rows: MessageRow[]) {
    const wanted = normalizeMessageId(search.q);
    if ((search.field !== 'any' && search.field !== 'id') || wanted === null) return;
    const exact = rows.find(row => row.id === wanted);
    const target = exact ?? (search.field === 'id' && rows.length === 1 ? rows[0] : undefined);
    if (target) void router.push({ name: 'app-message', params: { appId: id, messageId: target.id } });
}

/** Reloads the first page while keeping the current rows (and keyboard focus) on screen. */
async function refresh() {
    const id = appId.value;
    const search = active.value;
    if (isRefreshing.value) return;
    // With no rows on screen (failed first load or rejected search) a refresh is a full load.
    if (messages.value === null) return load();
    // Invalidates any in-flight older page; its rows would belong to the previous listing.
    const seq = ++loadSeq;
    const signal = beginRequest();
    isRefreshing.value = true;
    isSearching.value = false;
    isLoadingMore.value = false;
    refreshError.value = null;
    moreError.value = null;
    clearRevealed();
    try {
        const page = await fetchPage(id, search, signal);
        if (seq !== loadSeq || isUnmounted) return;
        messages.value = page.messages;
        nextCursor.value = page.nextCursor;
        loadError.value = null;
        validationHint.value = null;
    } catch (err) {
        if (seq === loadSeq && !isUnmounted) {
            const description = describe(err, search);
            refreshError.value = { ...description, message: `${description.message} The rows below are from the last time they loaded.` };
        }
    } finally {
        if (seq === loadSeq) isRefreshing.value = false;
    }
}

async function loadMore() {
    const id = appId.value;
    const search = active.value;
    const cursor = nextCursor.value;
    if (!cursor || isLoadingMore.value) return;
    const seq = loadSeq;
    const signal = controller?.signal ?? beginRequest();
    isLoadingMore.value = true;
    moreError.value = null;
    try {
        const page = await fetchPage(id, search, signal, cursor);
        if (seq !== loadSeq || isUnmounted) return;
        // Keyset pages do not overlap, but guard against duplicates if rows shift between requests.
        const seen = new Set((messages.value ?? []).map(m => m.id));
        messages.value = [...(messages.value ?? []), ...page.messages.filter(m => !seen.has(m.id))];
        nextCursor.value = page.nextCursor;
    } catch (err) {
        // Keep the loaded rows and the cursor so the same page can be retried.
        if (seq === loadSeq && !isUnmounted) moreError.value = describe(err, search);
    } finally {
        if (seq === loadSeq) isLoadingMore.value = false;
    }
}

// ---- Protected subjects ----

function revealedOf(id: string): Revealed | undefined {
    return revealed.value.get(id);
}

function revealedText(id: string): string | null {
    const entry = revealed.value.get(id);
    return entry?.kind === 'text' ? entry.text : null;
}

function clearRevealed() {
    revealed.value = new Map();
    revealing.value = new Set();
    revealError.value = null;
}

function hideSubject(id: string) {
    const next = new Map(revealed.value);
    next.delete(id);
    revealed.value = next;
}

async function reveal(row: MessageRow) {
    const id = appId.value;
    if (revealing.value.has(row.id) || authState.status !== 'signed-in') return;
    const seq = loadSeq;
    const confirmed = await confirmProtectedAccess('subject', { enhancedProtection: row.enhancedProtection });
    if (!confirmed || isUnmounted || authState.status !== 'signed-in' || seq !== loadSeq) return;
    const isCurrent = () => !isUnmounted && seq === loadSeq && authState.status === 'signed-in';
    revealing.value = new Set(revealing.value).add(row.id);
    revealError.value = null;
    try {
        const response = await DefaultApi.getAppMessageSubject({ path: { appId: id, messageId: row.id } });
        const data = response.data;
        if (!isCurrent()) return;
        if (typeof data?.subject !== 'string') throw new InvalidResponseError();
        if (messages.value?.some(m => m.id === row.id)) {
            revealed.value = new Map(revealed.value).set(row.id, { kind: 'text', text: data.subject });
        }
    } catch (err) {
        if (!isCurrent()) return;
        if (apiErrorStatus(err) === 409) {
            if (messages.value?.some(m => m.id === row.id)) revealed.value = new Map(revealed.value).set(row.id, { kind: 'none' });
        } else {
            const description = describeApiError(err, {
                notFound: 'That message wasn\'t found.',
                unavailable: 'Revealing the subject is temporarily unavailable. Try again in a moment.'
            });
            revealError.value = row.enhancedProtection ? withAuditSuffix(description) : description;
        }
    } finally {
        if (seq === loadSeq) {
            const next = new Set(revealing.value);
            next.delete(row.id);
            revealing.value = next;
        }
    }
}
</script>

<style lang="scss" scoped>
.header-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
}

.messages-body {
    display: flex;
    flex-direction: column;
    gap: 10px;
}

.filter-bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;

    .select {
        flex: none;
    }
}

.search-input {
    position: relative;
    display: flex;
    flex: 1;
    align-items: center;
    min-width: min(100%, 220px);

    > i {
        position: absolute;
        left: 10px;
        color: var(--text-3);
        font-size: 12px;
        pointer-events: none;
    }

    .input {
        width: 100%;
        padding-left: 30px;
        padding-right: 34px;
    }

    .btn {
        position: absolute;
        right: 3px;
    }
}

.search-status {
    font-size: 12px;
}

.validation {
    color: var(--warning);
}

.is-stale {
    opacity: 0.6;
}

.when {
    white-space: nowrap;
}

.messages-table :is(th, td) {
    padding-inline: 8px;
}

.id code {
    font-size: 12px;
}

.to,
.subject,
.sender-cell {
    white-space: nowrap;
}

.to > *,
.subject > * {
    vertical-align: middle;
}

.to .tag,
.subject .tag,
.subject .link-btn,
.subject .btn {
    margin-left: 6px;
}

.addr {
    display: inline-flex;
    max-width: 200px;
    min-width: 0;
    white-space: nowrap;

    > * {
        min-width: 0;
        overflow: hidden;
        text-overflow: ellipsis;
    }

    .local {
        flex: 0 0 auto;
        max-width: 90px;
    }

    .at {
        flex: none;
        overflow: visible;
    }
}

.domain-filter {
    max-width: 100%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;

    &.sender {
        display: block;
        max-width: 130px;
    }
}

.ellipsis {
    display: inline-block;
    max-width: 170px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
}

.protected-subject {
    white-space: nowrap;
}

.status {
    white-space: nowrap;

    .ep-icon {
        margin-left: 8px;
        color: var(--info);
        vertical-align: middle;
    }
}

.deliveries {
    white-space: nowrap;

    .tag {
        margin-left: 6px;
    }
}

.pager {
    padding-top: 12px;
}

@media (max-width: 640px) {
    .filter-bar .select {
        width: 100%;
    }
}
</style>
