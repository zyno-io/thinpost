<template>
    <div id="app-suppressions">
        <section class="card" aria-labelledby="suppressions-heading">
            <div class="card-header">
                <div class="heading-with-help">
                    <h2 id="suppressions-heading">Suppression list</h2>
                    <HelpButton topic="suppression list">
                        <p>Thinpost checks this list before every send. Recipients on it are skipped and nothing is sent to them.</p>
                        <p>Changes only affect future sends. Messages already skipped are not sent later.</p>
                        <p>Thinpost adds an address on its own only when the recipient's mail server permanently rejects the address itself, for example because it doesn't exist.</p>
                    </HelpButton>
                </div>
                <div class="header-actions">
                    <button type="button" class="btn sm" :disabled="busy || isLoading || isRefreshing" @click="refresh">
                        <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isRefreshing }" aria-hidden="true" />
                        {{ isRefreshing ? 'Refreshing…' : 'Refresh' }}
                    </button>
                    <button type="button" class="btn sm primary" :disabled="!canAct" @click="openAdd">
                        <i class="fa-solid fa-plus" aria-hidden="true" />Add
                    </button>
                </div>
            </div>

            <div class="card-body suppressions-body">
                <form class="filter-bar" role="search" aria-label="Search suppressions" @submit.prevent="submitSearch">
                    <select class="select" aria-label="Reason" :value="reason" @change="onReasonChange">
                        <option value="">All reasons</option>
                        <option v-for="r in SUPPRESSION_REASONS" :key="r" :value="r">{{ reasonLabel(r).label }}</option>
                    </select>
                    <div class="search-input">
                        <i class="fa-solid fa-magnifying-glass" aria-hidden="true" />
                        <input
                            ref="inputEl"
                            class="input"
                            type="search"
                            aria-label="Search suppressions"
                            autocomplete="off"
                            :spellcheck="false"
                            :maxlength="SEARCH_MAX_LENGTH + 1"
                            placeholder="Start of an address, or @domain"
                            :value="draft"
                            @input="onQueryInput"
                        />
                        <button v-if="draft" type="button" class="btn ghost sm icon" aria-label="Clear search" @click="clearSearch">
                            <i class="fa-solid fa-xmark" aria-hidden="true" />
                        </button>
                    </div>
                    <button v-if="selected.size" type="button" class="btn sm danger" :disabled="!canAct" @click="removeSelected">
                        Remove {{ selected.size }}
                    </button>
                </form>

                <ApiErrorNotice v-if="refreshError" :error="refreshError" :retry="refresh" />
                <ApiErrorNotice v-if="actionError" :error="actionError" />
            </div>

            <div v-if="isLoading" class="card-body"><Loader label="Loading suppressions" /></div>

            <div v-else-if="loadError" class="card-body"><ApiErrorNotice :error="loadError" :retry="load" /></div>

            <div v-else-if="rows && !rows.length" class="card-body empty">
                <i class="fa-solid fa-ban" aria-hidden="true" />
                <h2>{{ isFiltered ? 'No matches' : 'No suppressed addresses' }}</h2>
            </div>

            <template v-else-if="rows">
                <div class="table-wrap" :class="{ 'is-stale': isRefreshing }" :aria-busy="isRefreshing">
                    <table class="data suppressions-table">
                        <caption class="sr-only">Suppressions for {{ app.name }}, newest first</caption>
                        <thead>
                            <tr>
                                <th scope="col" class="check">
                                    <input
                                        type="checkbox"
                                        aria-label="Select all loaded"
                                        :checked="allSelected"
                                        :disabled="!canAct"
                                        @change="toggleAll"
                                    />
                                </th>
                                <th scope="col">Address</th>
                                <th scope="col">Reason</th>
                                <th scope="col">Added by</th>
                                <th scope="col">Added</th>
                                <th scope="col">From message</th>
                                <th scope="col" class="actions"><span class="sr-only">Actions</span></th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="row in rows" :key="row.id">
                                <td class="check">
                                    <input
                                        type="checkbox"
                                        :aria-label="`Select ${row.address}`"
                                        :checked="selected.has(row.id)"
                                        :disabled="!canAct"
                                        @change="toggle(row.id)"
                                    />
                                </td>
                                <th scope="row" class="address">
                                    <span class="trunc" :title="row.address">{{ row.address }}</span>
                                </th>
                                <td class="nowrap">
                                    <span class="tag" :class="tagClass(reasonLabel(row.reason).tone)">{{ reasonLabel(row.reason).label }}</span>
                                </td>
                                <td class="nowrap">{{ originLabel(row.origin) }}</td>
                                <td class="nowrap">
                                    <time :datetime="row.created_at">{{ formatDateTime(row.created_at) }}</time>
                                </td>
                                <td class="nowrap">
                                    <RouterLink
                                        v-if="row.source_message_id"
                                        :to="{ name: 'app-message', params: { appId: app.id, messageId: row.source_message_id } }"
                                        :title="row.source_message_id"
                                        :aria-label="`Message ${row.source_message_id}`"
                                    >
                                        <code>{{ shortId(row.source_message_id, 8) }}…</code>
                                    </RouterLink>
                                    <span v-else class="muted" aria-hidden="true">—</span>
                                </td>
                                <td class="actions">
                                    <button type="button" class="btn sm" :disabled="!canAct" @click="removeRows([row])">
                                        Remove<span class="sr-only"> {{ row.address }}</span>
                                    </button>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>

                <div v-if="moreError" class="card-body"><ApiErrorNotice :error="moreError" :retry="loadMore" /></div>

                <div class="card-body pager">
                    <span class="muted" role="status">{{ rows.length }} loaded{{ nextCursor ? '' : ' · end' }}</span>
                    <button v-if="nextCursor" type="button" class="btn" :disabled="isLoadingMore || isRefreshing" @click="loadMore">
                        {{ isLoadingMore ? 'Loading…' : 'Load more' }}
                    </button>
                </div>
            </template>
        </section>

        <SuppressionAddModal v-if="adding" :is-submitting="isAdding" :error="addError" @close="closeAdd" @submit="submitAdd" />
    </div>
</template>

<script lang="ts" setup>
import { showConfirmDestroy, showToast } from '@zyno-io/vue-foundation';
import { computed, nextTick, onBeforeUnmount, ref, shallowRef, watch } from 'vue';

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { formatDateTime, shortId } from '@/format';
import { SEARCH_DEBOUNCE_MS } from '@/message-search';
import { DefaultApi, type Suppression } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import SuppressionAddModal from '@/shared/components/suppression-add-modal.vue';
import { tagClass } from '@/states';
import { savedSuppressionSearch, saveSuppressionSearch } from '@/suppression-search-store';
import { originLabel, parseSuppressionQuery, reasonLabel, SEARCH_MAX_LENGTH, SUPPRESSION_REASONS, type SuppressionReason } from '@/suppressions';

const PAGE_SIZE = 50;
const MAX_DELETE = 100;
const MAX_CURSOR_LENGTH = 512;
const REMOVE_TEXT = 'Thinpost will send to these addresses again. Messages already skipped are not sent later.';

/** A spam complaint or unsubscribe is the recipient's own request, and only the console may undo it. */
function isRecipientRequested(row: Suppression) {
    return row.origin === 'recipient' && (row.reason === 'spam_complaint' || row.reason === 'unsubscribe');
}

function recipientRequestedText(requested: number, total: number) {
    const lead = total === 1 ? 'The recipient asked not to receive this email.' : requested === 1 ? 'One recipient asked not to receive this email.' : `${requested} recipients asked not to receive this email.`;
    return `${lead} Remove anyway? ${REMOVE_TEXT}`;
}
const UNAVAILABLE = 'The suppression list is temporarily unavailable. Try again in a moment.';

const { app, appId } = useAppContext();
const inputEl = ref<HTMLInputElement | null>(null);

// Search terms never leave memory: not the URL, router state, history state or browser storage.
const draft = ref(savedSuppressionSearch(appId.value));
const active = ref(draft.value);
const reason = ref<SuppressionReason | ''>('');

const rows = shallowRef<Suppression[] | null>(null);
const nextCursor = ref<string | null>(null);
const isLoading = ref(true);
const isLoadingMore = ref(false);
const isRefreshing = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const moreError = ref<ApiErrorDescription | null>(null);
const refreshError = ref<ApiErrorDescription | null>(null);
const actionError = ref<ApiErrorDescription | null>(null);
const selected = ref(new Set<string>());

const adding = ref(false);
const isAdding = ref(false);
const addError = ref<ApiErrorDescription | null>(null);
const isRemoving = ref(false);

const busy = computed(() => isRemoving.value || isAdding.value);
const canAct = computed(() => authState.status === 'signed-in' && !busy.value && !isRefreshing.value);
const isFiltered = computed(() => parseSuppressionQuery(active.value) !== null || reason.value !== '');
const allSelected = computed(() => !!rows.value?.length && rows.value.every(r => selected.value.has(r.id)));

let loadSeq = 0;
let controller: AbortController | null = null;
let timer: ReturnType<typeof setTimeout> | undefined;
let isUnmounted = false;

watch([appId, active, reason], () => void load(), { immediate: true });
onBeforeUnmount(() => {
    isUnmounted = true;
    clearTimeout(timer);
    controller?.abort();
});

// ---- Search ----

function applyDraft() {
    saveSuppressionSearch(appId.value, draft.value.trim());
    active.value = draft.value.trim();
}

function onQueryInput(event: Event) {
    draft.value = (event.target as HTMLInputElement).value;
    clearTimeout(timer);
    timer = setTimeout(applyDraft, SEARCH_DEBOUNCE_MS);
}

function submitSearch() {
    clearTimeout(timer);
    const before = loadSeq;
    applyDraft();
    void nextTick(() => {
        if (before === loadSeq) void load();
    });
}

function clearSearch() {
    clearTimeout(timer);
    draft.value = '';
    applyDraft();
    void nextTick(() => inputEl.value?.focus());
}

function onReasonChange(event: Event) {
    reason.value = (event.target as HTMLSelectElement).value as SuppressionReason | '';
}

// ---- Loading ----

interface Page {
    suppressions: Suppression[];
    nextCursor: string | null;
}

async function fetchPage(id: string, q: string, filter: SuppressionReason | '', signal: AbortSignal, cursor?: string): Promise<Page> {
    const query = parseSuppressionQuery(q);
    const response = query
        ? await DefaultApi.searchAppSuppressions({
              path: { appId: id },
              body: { q: query.q, field: query.field, limit: PAGE_SIZE, ...(filter ? { reason: filter } : {}), ...(cursor ? { cursor } : {}) },
              signal
          })
        : await DefaultApi.listAppSuppressions({
              path: { appId: id },
              query: { limit: PAGE_SIZE, ...(filter ? { reason: filter } : {}), ...(cursor ? { cursor } : {}) }
          });
    const data = response.data;
    if (!Array.isArray(data?.suppressions)) throw new InvalidResponseError();
    for (const row of data.suppressions) {
        if (!row || typeof row.id !== 'string' || typeof row.address !== 'string' || typeof row.created_at !== 'string') throw new InvalidResponseError();
    }
    const next = data.next_cursor;
    if (next !== undefined && (typeof next !== 'string' || next.length > MAX_CURSOR_LENGTH)) throw new InvalidResponseError();
    return { suppressions: data.suppressions, nextCursor: next || null };
}

function describe(err: unknown): ApiErrorDescription {
    return describeApiError(err, { notFound: 'The app was not found.', unavailable: UNAVAILABLE });
}

function beginRequest(): AbortSignal {
    controller?.abort();
    controller = new AbortController();
    return controller.signal;
}

function resetTransient() {
    selected.value = new Set();
    moreError.value = null;
    refreshError.value = null;
    isLoadingMore.value = false;
    isRefreshing.value = false;
}

async function load() {
    const id = appId.value;
    const seq = ++loadSeq;
    const signal = beginRequest();
    isLoading.value = true;
    loadError.value = null;
    actionError.value = null;
    resetTransient();
    try {
        const page = await fetchPage(id, active.value, reason.value, signal);
        if (seq !== loadSeq || isUnmounted) return;
        rows.value = page.suppressions;
        nextCursor.value = page.nextCursor;
    } catch (err) {
        if (seq !== loadSeq || isUnmounted) return;
        rows.value = null;
        nextCursor.value = null;
        loadError.value = apiErrorStatus(err) === 400 ? { kind: 'invalid', status: 400, message: 'That search can\'t be used. Check the text and try again.' } : describe(err);
    } finally {
        if (seq === loadSeq) isLoading.value = false;
    }
}

/** Reloads the first page while keeping the current rows on screen. */
async function refresh() {
    if (rows.value === null) return load();
    const id = appId.value;
    // Supersedes anything in flight (a load, an older refresh or a page); only the newest result is painted.
    const seq = ++loadSeq;
    const signal = beginRequest();
    isLoading.value = false;
    isRefreshing.value = true;
    isLoadingMore.value = false;
    refreshError.value = null;
    moreError.value = null;
    try {
        const page = await fetchPage(id, active.value, reason.value, signal);
        if (seq !== loadSeq || isUnmounted) return;
        rows.value = page.suppressions;
        nextCursor.value = page.nextCursor;
        const present = new Set(page.suppressions.map(r => r.id));
        selected.value = new Set([...selected.value].filter(sid => present.has(sid)));
    } catch (err) {
        if (seq === loadSeq && !isUnmounted) {
            const description = describe(err);
            refreshError.value = { ...description, message: `${description.message} The rows below are from the last time they loaded.` };
        }
    } finally {
        if (seq === loadSeq) isRefreshing.value = false;
    }
}

async function loadMore() {
    const id = appId.value;
    const cursor = nextCursor.value;
    if (!cursor || isLoadingMore.value) return;
    const seq = loadSeq;
    const signal = controller?.signal ?? beginRequest();
    isLoadingMore.value = true;
    moreError.value = null;
    try {
        const page = await fetchPage(id, active.value, reason.value, signal, cursor);
        if (seq !== loadSeq || isUnmounted) return;
        const seen = new Set((rows.value ?? []).map(r => r.id));
        rows.value = [...(rows.value ?? []), ...page.suppressions.filter(r => !seen.has(r.id))];
        nextCursor.value = page.nextCursor;
    } catch (err) {
        if (seq === loadSeq && !isUnmounted) moreError.value = describe(err);
    } finally {
        if (seq === loadSeq) isLoadingMore.value = false;
    }
}

// ---- Selection ----

function toggle(rowId: string) {
    const next = new Set(selected.value);
    if (!next.delete(rowId)) next.add(rowId);
    selected.value = next;
}

function toggleAll() {
    selected.value = allSelected.value ? new Set() : new Set((rows.value ?? []).map(r => r.id));
}

// ---- Add ----

function openAdd() {
    if (!canAct.value) return;
    addError.value = null;
    adding.value = true;
}

function closeAdd() {
    if (isAdding.value) return;
    adding.value = false;
}

async function submitAdd(addresses: string[]) {
    if (!canAct.value) return;
    const id = appId.value;
    isAdding.value = true;
    addError.value = null;
    try {
        const response = await DefaultApi.addAppSuppressions({ path: { appId: id }, body: { addresses } });
        const results = response.data?.results;
        if (!Array.isArray(results)) throw new InvalidResponseError();
        const added = results.filter(r => r.status === 'added').length;
        const exists = results.filter(r => r.status === 'exists').length;
        adding.value = false;
        showToast({ message: `Added ${added} · already on the list ${exists}`, durationSecs: 4 });
        if (!isUnmounted && id === appId.value) void refresh();
    } catch (err) {
        const status = apiErrorStatus(err);
        addError.value =
            status === 400
                ? { kind: 'invalid', status: 400, message: 'Thinpost rejected one or more of the addresses. Check that each one is a valid email address.' }
                : describeApiError(err, { mutation: true, notFound: 'The app was not found.', unavailable: UNAVAILABLE });
        // No definitive answer: the addresses may have been added.
        if (!isUnmounted && (status === undefined || status >= 500 || err instanceof InvalidResponseError)) void refresh();
    } finally {
        isAdding.value = false;
    }
}

// ---- Remove ----

function removeSelected() {
    const chosen = (rows.value ?? []).filter(r => selected.value.has(r.id));
    return removeRows(chosen);
}

async function removeRows(chosen: Suppression[]) {
    if (!canAct.value || !chosen.length) return;
    const id = appId.value;
    const seq = loadSeq;
    const requested = chosen.filter(isRecipientRequested).length;
    const title = chosen.length === 1 ? 'Remove from suppression list' : `Remove ${chosen.length} from suppression list`;
    const confirmed = await showConfirmDestroy(title, requested ? recipientRequestedText(requested, chosen.length) : REMOVE_TEXT);
    if (!confirmed || isUnmounted || seq !== loadSeq || !canAct.value) return;
    isRemoving.value = true;
    actionError.value = null;
    let reload = true;
    let deleted = 0;
    try {
        // The server accepts at most 100 IDs per request.
        for (let at = 0; at < chosen.length; at += MAX_DELETE) {
            const response = await DefaultApi.deleteAppSuppressions({ path: { appId: id }, body: { ids: chosen.slice(at, at + MAX_DELETE).map(r => r.id) } });
            if (typeof response.data?.deleted !== 'number') throw new InvalidResponseError();
            deleted += response.data.deleted;
        }
        if (!isUnmounted && id === appId.value) showToast({ message: `Removed ${deleted}`, durationSecs: 3 });
    } catch (err) {
        if (isUnmounted || id !== appId.value) return;
        const status = apiErrorStatus(err);
        actionError.value = describeApiError(err, { mutation: true, notFound: 'The app was not found.', unavailable: UNAVAILABLE });
        if (deleted > 0) showToast({ message: `Removed ${deleted}`, durationSecs: 3 });
        // Only a definitive client-side refusal of the first batch leaves the list unchanged; otherwise reload to show the truth.
        if (deleted === 0 && (status === 400 || status === 401 || status === 403)) reload = false;
    } finally {
        isRemoving.value = false;
    }
    if (reload && !isUnmounted && id === appId.value) await refresh();
}
</script>

<style lang="scss" scoped>
#app-suppressions {
    display: flex;
    flex-direction: column;
    gap: 16px;
}

.header-actions {
    display: flex;
    gap: 8px;
}

.suppressions-body {
    display: flex;
    flex-direction: column;
    gap: 10px;

    &:not(:has(*)) {
        display: none;
    }
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

.is-stale {
    opacity: 0.6;
}

.suppressions-table :is(th, td) {
    padding-inline: 8px;
}

.check {
    width: 1%;
}

.nowrap {
    white-space: nowrap;
}

.address {
    white-space: nowrap;
    font-weight: 400;
}

.trunc {
    display: inline-block;
    max-width: 280px;
    overflow: hidden;
    text-overflow: ellipsis;
    vertical-align: bottom;
    white-space: nowrap;
}

.actions {
    text-align: right;
    white-space: nowrap;
}

code {
    font-size: 12px;
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
