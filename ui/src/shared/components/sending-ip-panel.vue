<template>
    <section class="card" aria-labelledby="sending-ip-heading" :aria-busy="isLoading || isSaving">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 id="sending-ip-heading">Sending IP</h2>
                <HelpButton topic="sending IP">
                    <p>The IP address this app's email is sent from. Mailbox providers judge mail partly by the address it comes from, so apps that must not affect each other's reputation can use different IPs.</p>
                    <ul>
                        <li>The <strong>main IP</strong> is used unless you choose another.</li>
                        <li>A change applies to mail sent from now on, including messages already waiting. A delivery already with the sending worker finishes from the address it started with.</li>
                        <li>If the chosen IP isn't available on the sending worker, this app's mail waits. It is never sent from a different address.</li>
                    </ul>
                    <p>Your Thinpost administrator names and manages the IPs on the Operations page.</p>
                </HelpButton>
            </div>
            <button type="button" class="btn sm" :disabled="isLoading || isSaving || !isSignedIn" @click="load">
                <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isLoading && !!current }" aria-hidden="true" />Refresh
            </button>
        </div>
        <div class="card-body panel-body">
            <Loader v-if="isLoading && !current" label="Loading sending IP" />
            <ApiErrorNotice v-if="loadError" :error="loadError" :retry="isSignedIn ? load : undefined" retry-label="Reload" />
            <template v-if="current">
                <p class="line" data-testid="current-ip">
                    <span>Sends from</span>
                    <strong>{{ currentLabel }}</strong>
                    <span v-if="currentRow" class="muted">· HELO name <code>{{ currentRow.helo_name }}</code></span>
                    <span v-if="workerState" class="tag" :class="workerState.tone" data-testid="worker-state" :title="workerState.title">{{ workerState.text }}</span>
                </p>
                <p v-if="currentRow && workerNote(currentRow)" class="hint warn" data-testid="worker-note">{{ workerNote(currentRow) }}</p>

                <p v-if="!pool" class="hint" data-testid="pool-unavailable">The list of sending IPs couldn't be loaded, so the IP can't be changed here right now.</p>
                <p v-else-if="!choices.length" class="hint" data-testid="no-ips">
                    No other sending IPs are named yet, so this app uses the main IP.
                    <RouterLink to="/operations">Name more on the Operations page.</RouterLink>
                </p>
                <div v-else class="line">
                    <label for="sending-ip-select" class="sr-only">Sending IP for this app</label>
                    <select id="sending-ip-select" v-model="choice" class="input" :disabled="isSaving || !isSignedIn" data-testid="sending-ip-select">
                        <option value="">{{ MAIN_IP_LABEL }} (default)</option>
                        <option v-for="ip in choices" :key="ip.id" :value="ip.id">{{ ip.name }}</option>
                    </select>
                    <button type="button" class="btn sm primary" :disabled="!changed || isSaving || !isSignedIn" data-testid="save-sending-ip" @click="save">
                        {{ isSaving ? 'Saving…' : 'Change IP' }}
                    </button>
                </div>
                <p v-if="!isSignedIn" class="hint">Sign in again to change the sending IP.</p>
            </template>
            <ApiErrorNotice v-if="saveError" :error="saveError" />
            <p v-if="savedNote" class="saved" role="status"><i class="fa-solid fa-check" aria-hidden="true" /> {{ savedNote }}</p>
        </div>
    </section>
</template>

<script setup lang="ts">
import { showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { DefaultApi, type AppSendingIp, type SendingIp, type SendingIps } from '@/openapi-client-generated';
import { MAIN_IP_LABEL, onWorker, parseSendingIps, workerNote } from '@/sending-ips';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import { isOpaqueId } from '@/validation';

const { app, appId } = useAppContext();

const current = shallowRef<AppSendingIp | null>(null);
const pool = shallowRef<SendingIps | null>(null);
const choice = ref('');
const isLoading = ref(false);
const isSaving = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const saveError = ref<ApiErrorDescription | null>(null);
const savedNote = ref<string | null>(null);

const isSignedIn = computed(() => authState.status === 'signed-in');

const currentId = computed(() => current.value?.sending_ip_id ?? '');
// An app with no assignment uses the main IP, so its record (if the main IP has one) is the row that counts.
const currentRow = computed<SendingIp | null>(() => pool.value?.ips.find(ip => (currentId.value ? ip.id === currentId.value : ip.is_primary)) ?? null);
const currentLabel = computed(() => {
    if (!current.value) return '';
    if (current.value.sending_ip_id === null) return MAIN_IP_LABEL;
    return current.value.name ?? 'Unknown';
});
// The main IP is the default choice, so its own record isn't a second entry, unless the app is pinned to it.
const choices = computed(() => (pool.value?.ips ?? []).filter(ip => !ip.is_primary || ip.id === currentId.value));
const changed = computed(() => choice.value !== currentId.value);

/** Where the chosen IP stands on the sending worker; the main IP with no record is by definition there. */
const workerState = computed(() => {
    const row = currentRow.value;
    if (!row) return current.value?.sending_ip_id === null && pool.value?.edge_reports_sources ? { text: 'Available', tone: 'success', title: 'The main IP is the sending worker\'s own address.' } : null;
    const state = onWorker(row);
    if (state === 'yes') return row.bounce_on_edge === false ? { text: 'Mail is waiting', tone: 'warning', title: workerNote(row) } : { text: 'Available', tone: 'success', title: 'The sending worker reported this address.' };
    if (state === 'no') return { text: 'Mail is waiting', tone: 'warning', title: 'The sending worker doesn\'t have this address.' };
    return { text: 'Not known', tone: '', title: 'The sending worker hasn\'t reported its addresses.' };
});

let seq = 0;
let isUnmounted = false;

async function load() {
    const mine = ++seq;
    isLoading.value = true;
    loadError.value = null;
    savedNote.value = null;
    const target = appId.value;
    try {
        const { data } = await DefaultApi.getAppSendingIp({ path: { appId: target } });
        if (!data || !(data.sending_ip_id === null || isOpaqueId(data.sending_ip_id)) || !(data.name === null || typeof data.name === 'string')) throw new InvalidResponseError();
        if (isUnmounted || mine !== seq) return;
        current.value = data;
        choice.value = data.sending_ip_id ?? '';
    } catch (err) {
        if (isUnmounted || mine !== seq) return;
        // Without a loaded value there is nothing safe to change.
        current.value = null;
        pool.value = null;
        loadError.value = describeApiError(err, { notFound: 'The app was not found.', unavailable: 'The sending IP is temporarily unavailable. Try again in a moment.' });
        isLoading.value = false;
        return;
    }
    // The list of IPs only adds names and choices; failing to load it leaves the assignment visible.
    try {
        const { data } = await DefaultApi.listSendingIps();
        if (isUnmounted || mine !== seq) return;
        pool.value = parseSendingIps(data);
    } catch {
        if (isUnmounted || mine !== seq) return;
        pool.value = null;
    } finally {
        if (mine === seq) isLoading.value = false;
    }
}

async function save() {
    const before = current.value;
    if (!before || !changed.value || isSaving.value) return;
    const target = app.value;
    const sendingIpId = choice.value || null;
    isSaving.value = true;
    saveError.value = null;
    savedNote.value = null;
    seq++;
    isLoading.value = false;
    try {
        const { data } = await DefaultApi.setAppSendingIp({ path: { appId: target.id }, body: { sending_ip_id: sendingIpId } });
        if (!data || !(data.sending_ip_id === null || isOpaqueId(data.sending_ip_id))) throw new InvalidResponseError();
        const label = data.sending_ip_id === null ? MAIN_IP_LABEL : (data.name ?? 'the chosen IP');
        if (isUnmounted) {
            showToast({ message: `The sending IP for “${target.name}” was changed to ${label} after you left the page.`, durationSecs: 8 });
            return;
        }
        current.value = data;
        choice.value = data.sending_ip_id ?? '';
        savedNote.value = `Saved. “${target.name}” now sends from ${label}, starting with its next delivery.`;
        showToast({ message: `Sending IP changed for “${target.name}”.`, durationSecs: 5 });
    } catch (err) {
        if (isUnmounted) {
            showToast({ message: `The sending IP change for “${target.name}” may not have completed. Open the app to check it.`, durationSecs: 8 });
            return;
        }
        const status = apiErrorStatus(err);
        saveError.value = describeApiError(err, {
            mutation: true,
            notFound: 'The app or that sending IP was not found. It may have been removed. Reload to check. Nothing was changed.',
            unavailable: 'Changing the sending IP is unavailable right now. Mail intake may be closed. Try again in a moment.'
        });
        // Without a definitive refusal the stored value may differ from the one shown.
        if (status === undefined || status >= 500) void load();
    } finally {
        isSaving.value = false;
    }
}

watch(appId, () => void load(), { immediate: true });

onBeforeUnmount(() => {
    isUnmounted = true;
});
</script>

<style lang="scss" scoped>
.card-header {
    flex-wrap: wrap;
}

.panel-body {
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

    &.warn {
        color: var(--warning, var(--text-2));
    }
}

.saved {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--success);
}
</style>
