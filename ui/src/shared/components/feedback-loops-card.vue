<template>
    <section class="card" aria-labelledby="feedback-loops-heading" :aria-busy="isLoading || isSaving">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 id="feedback-loops-heading">
                    Feedback loops
                    <template v-if="data">
                        <span v-if="!data.mailbox" class="tag">No mailbox</span>
                        <span v-else-if="!data.reporters.length" class="tag warning" data-testid="no-reporters">Accepting nothing</span>
                        <span v-else class="tag success">Receiving</span>
                    </template>
                </h2>
                <HelpButton topic="feedback loops">
                    <p>When a recipient marks your email as spam, their mailbox provider can send an abuse report (ARF) to an address you register. Thinpost then adds that recipient to the app's suppression list and sends a <code>complained</code> webhook event.</p>
                    <ol>
                        <li>Create the mailbox and copy its address.</li>
                        <li>Add the domains that sign the reports (their DKIM domains). Yahoo signs as <code>arf.mail.yahoo.com</code>.</li>
                        <li>Yahoo Complaint Feedback Loop: in Sender Hub, register each app's sending domain with this address.</li>
                        <li>Microsoft JMRP: in SNDS, register your sending IP and this address, then complete the ownership confirmation. Thinpost can only confirm Microsoft's report format after the first real report arrives.</li>
                        <li>Gmail has no per-message feedback loop. Use Postmaster Tools to see overall spam rates.</li>
                    </ol>
                    <p>A report counts only if it is signed by a listed domain and matches a message Thinpost delivered. Anything else just adds to a counter below. Report contents are never stored. Many <code>Unauthenticated</code> usually means the domain list is wrong. Many <code>Not matched</code> usually means the provider removes details of the original message.</p>
                </HelpButton>
            </div>
            <div class="actions">
                <button type="button" class="btn sm" :disabled="isLoading || isSaving" @click="load">
                    <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isLoading && !!data }" aria-hidden="true" />Refresh
                </button>
            </div>
        </div>
        <div class="card-body rows">
            <Loader v-if="isLoading && !data" label="Loading feedback loops" />
            <ApiErrorNotice v-if="loadError" :error="loadError" :retry="load" retry-label="Reload" />
            <ApiErrorNotice v-if="actionError" :error="actionError" />
            <template v-if="data">
                <div v-if="data.mailbox" class="mailbox">
                    <CopyField v-if="data.mailbox.address" label="Mailbox address" :value="data.mailbox.address" data-testid="mailbox-address" />
                    <p v-else class="hint" data-testid="no-address">Thinpost has no bounce domain set up, so the address can't be shown. Ask your Thinpost administrator.</p>
                    <p class="hint">Created <time :datetime="data.mailbox.created_at">{{ formatDateTime(data.mailbox.created_at) }}</time></p>
                    <div class="line">
                        <button type="button" class="btn sm" :disabled="!!blocker || isSaving" :title="blocker ?? undefined" data-testid="rotate" @click="rotate">
                            <i class="fa-solid fa-arrows-rotate" aria-hidden="true" />Replace address
                        </button>
                    </div>
                </div>
                <div v-else class="line">
                    <button type="button" class="btn sm primary" :disabled="!!blocker || !data.receiving || isSaving" :title="blocker ?? undefined" data-testid="create-mailbox" @click="create">
                        <i class="fa-solid fa-inbox" aria-hidden="true" />Create mailbox
                    </button>
                    <span v-if="!data.receiving" class="hint">Unavailable: Thinpost has no bounce domain set up. Ask your Thinpost administrator.</span>
                </div>

                <div class="reporters">
                    <p class="line">
                        <strong>Reporter domains</strong>
                        <code v-for="domain in data.reporters" :key="domain" class="tag">{{ domain }}</code>
                        <span v-if="!data.reporters.length" class="muted" data-testid="reporters-empty">none, so no reports are accepted</span>
                        <button type="button" class="btn sm" :disabled="!!blocker || isSaving" :title="blocker ?? undefined" data-testid="edit-reporters" @click="isEditing = true">
                            <i class="fa-solid fa-pen" aria-hidden="true" />Edit
                        </button>
                    </p>
                </div>

                <div class="table-wrap">
                    <table class="data compact" data-testid="feedback-counts">
                        <caption class="sr-only">Feedback reports received in the last 14 days</caption>
                        <thead>
                            <tr><th scope="col">Result</th><th scope="col" class="num">Today (UTC)</th><th scope="col" class="num">14 days</th></tr>
                        </thead>
                        <tbody>
                            <tr v-for="row in rows" :key="row.result" :data-result="row.result">
                                <th scope="row" class="nowrap" :title="FEEDBACK_RESULT_INFO[row.result].help">{{ FEEDBACK_RESULT_INFO[row.result].label }}</th>
                                <td class="num" :class="{ zero: row.today === 0 }">{{ formatCount(row.today) }}</td>
                                <td class="num" :class="{ zero: row.total === 0 }">{{ formatCount(row.total) }}</td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </template>
        </div>

        <FeedbackReportersModal
            v-if="isEditing && data"
            :current="data.reporters"
            :is-submitting="isSaving"
            :error="saveError"
            @close="closeEditor"
            @submit="saveReporters"
        />
    </section>
</template>

<script setup lang="ts">
import { showConfirmDestroy, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, onMounted, ref, shallowRef } from 'vue';

import { describeApiError, type ApiErrorDescription } from '@/api-errors';
import { authState } from '@/auth';
import { countRows, FEEDBACK_RESULT_INFO, parseFeedbackLoops } from '@/feedback-loops';
import { formatDateTime } from '@/format';
import { DefaultApi, type FeedbackLoops } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import CopyField from '@/shared/components/copy-field.vue';
import FeedbackReportersModal from '@/shared/components/feedback-reporters-modal.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import { formatCount } from '@/submission-rate';

const props = defineProps<{
    /** Mail intake mode from the operations snapshot; changes need it open. Null when unknown. */
    gateMode: string | null;
}>();

const data = shallowRef<FeedbackLoops | null>(null);
const isLoading = ref(false);
const isSaving = ref(false);
const isEditing = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const actionError = ref<ApiErrorDescription | null>(null);
const saveError = ref<ApiErrorDescription | null>(null);

let seq = 0;
let isUnmounted = false;

const blocker = computed(() => {
    if (authState.status !== 'signed-in') return 'Sign in again to change feedback loops.';
    if (props.gateMode !== null && props.gateMode !== 'open') return 'Changes are only possible while mail intake is open.';
    return null;
});
const rows = computed(() => countRows(data.value?.counts ?? [], new Date()));

async function load() {
    const mine = ++seq;
    isLoading.value = true;
    loadError.value = null;
    try {
        const { data: body } = await DefaultApi.getFeedbackLoops();
        if (isUnmounted || mine !== seq) return;
        data.value = parseFeedbackLoops(body);
    } catch (err) {
        if (isUnmounted || mine !== seq) return;
        data.value = null;
        loadError.value = describeApiError(err, { unavailable: 'Feedback loops are temporarily unavailable. Try again in a moment.' });
    } finally {
        if (mine === seq) isLoading.value = false;
    }
}

async function createMailbox(rotate: boolean) {
    if (blocker.value || isSaving.value) return;
    isSaving.value = true;
    actionError.value = null;
    seq++;
    isLoading.value = false;
    try {
        await DefaultApi.createFeedbackMailbox({ body: rotate ? { rotate: true } : {} });
        if (isUnmounted) {
            showToast({ message: `The feedback mailbox was ${rotate ? 'replaced' : 'created'} after you left the page. Open Operations to see the address.`, durationSecs: 8 });
            return;
        }
        showToast({ message: rotate ? 'Feedback mailbox replaced. Register the new address with every feedback loop.' : 'Feedback mailbox created.', durationSecs: 6 });
    } catch (err) {
        if (isUnmounted) return;
        actionError.value = describeApiError(err, {
            mutation: true,
            conflict: 'A mailbox already exists, or Thinpost has no bounce domain set up. Reload to check.',
            unavailable: 'Changing the mailbox is unavailable right now. Mail intake may be closed. Try again in a moment.'
        });
    } finally {
        isSaving.value = false;
    }
    if (!isUnmounted) await load();
}

const create = () => createMailbox(false);

async function rotate() {
    if (blocker.value || isSaving.value) return;
    const ok = await showConfirmDestroy(
        'Replace the feedback mailbox?',
        'The current address stops receiving immediately, and reports sent to it are lost. You need to register the new address with every feedback loop (Yahoo, Microsoft and any others).'
    );
    if (!ok || isUnmounted) return;
    await createMailbox(true);
}

async function saveReporters(domains: string[]) {
    if (blocker.value || isSaving.value) return;
    isSaving.value = true;
    saveError.value = null;
    seq++;
    isLoading.value = false;
    let saved = false;
    try {
        await DefaultApi.setFeedbackReporters({ body: { domains } });
        saved = true;
    } catch (err) {
        if (!isUnmounted) {
            saveError.value = describeApiError(err, {
                mutation: true,
                unavailable: 'Changing the reporter domains is unavailable right now. Mail intake may be closed. Try again in a moment.'
            });
        }
    } finally {
        isSaving.value = false;
    }
    if (isUnmounted) return;
    if (saved) {
        isEditing.value = false;
        showToast({ message: 'Reporter domains saved.', durationSecs: 5 });
    }
    await load();
}

function closeEditor() {
    if (!isSaving.value) isEditing.value = false;
}

onMounted(() => void load());
onBeforeUnmount(() => {
    isUnmounted = true;
});
</script>

<style lang="scss" scoped>
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

.hint {
    color: var(--text-3);
    font-size: 12px;
}

.mailbox {
    display: flex;
    flex-direction: column;
    gap: 6px;
}

.nowrap {
    white-space: nowrap;
}

.zero {
    color: var(--text-3);
}
</style>
