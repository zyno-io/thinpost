<template>
    <section class="card" aria-labelledby="sending-ips-heading" :aria-busy="isLoading || isSaving">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 id="sending-ips-heading">Sending IPs</h2>
                <HelpButton topic="sending IPs">
                    <p>Mail leaves Thinpost from an IP address. Giving different apps different IPs keeps one app's reputation with mailbox providers from affecting another's.</p>
                    <ul>
                        <li>The <strong>main IP</strong> is the sending worker's first address. Every app uses it unless you choose another IP for it.</li>
                        <li>Which addresses exist is set up on the sending worker. Here you give each one a name, the HELO name it introduces itself with, and a bounce domain. Adding an address here doesn't give the worker that address.</li>
                        <li>If an app uses an IP the sending worker doesn't have, its mail waits. It is never sent from a different address.</li>
                        <li>Changes apply to mail sent from now on. A delivery already with the sending worker finishes from the address it started with.</li>
                        <li>Warm up a new IP by moving one low-volume app to it first.</li>
                    </ul>
                    <p>For each address, ask whoever manages your DNS to set its reverse DNS to the HELO name, to point the HELO name at the address, and to list the address in the SPF record of its bounce domain (the table at the bottom shows what to add). The DNS check reads public DNS and doesn't change anything.</p>
                </HelpButton>
            </div>
            <div class="actions">
                <button type="button" class="btn sm" :disabled="isLoading || isSaving" @click="load">
                    <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isLoading && !!data }" aria-hidden="true" />Refresh
                </button>
                <button type="button" class="btn sm primary" :disabled="!!blocker || !data || isSaving" :title="blocker ?? undefined" data-testid="add-ip" @click="openAdd()">
                    <i class="fa-solid fa-plus" aria-hidden="true" />Add IP
                </button>
            </div>
        </div>
        <div class="card-body rows">
            <Loader v-if="isLoading && !data" label="Loading sending IPs" />
            <ApiErrorNotice v-if="loadError" :error="loadError" :retry="load" retry-label="Reload" />
            <ApiErrorNotice v-if="actionError" :error="actionError" />
            <template v-if="data">
                <p v-if="blocker" class="hint" data-testid="blocker">{{ blocker }}</p>
                <p v-if="!data.edge_reported_at" class="hint" data-testid="no-report">The sending worker hasn't reported its addresses yet, so whether each IP is available to it isn't known.</p>
                <p v-else-if="!data.edge_reports_sources" class="hint" data-testid="no-sources">The sending worker doesn't report its addresses (it may be an older version), so whether each IP is available to it isn't known. It can send only from its main address.</p>
                <p v-else class="hint" data-testid="reported-at">The sending worker last reported its addresses <time :datetime="data.edge_reported_at">{{ formatDateTime(data.edge_reported_at) }}</time>.</p>

                <p v-if="!data.ips.length && !data.unnamed_edge_addresses.length" class="muted" data-testid="empty">
                    No sending IPs are named yet. Every app sends from the main IP{{ data.global_bounce_domain ? `, and its bounces come back to ${data.global_bounce_domain}` : '' }}.
                </p>

                <div v-if="data.ips.length || data.unnamed_edge_addresses.length" class="table-wrap">
                    <table class="data compact" data-testid="sending-ips">
                        <caption class="sr-only">Sending IPs</caption>
                        <thead>
                            <tr>
                                <th scope="col">Name</th>
                                <th scope="col">IP address</th>
                                <th scope="col">HELO name</th>
                                <th scope="col">Bounce domain</th>
                                <th scope="col" class="num">Apps using it</th>
                                <th scope="col">On the sending worker</th>
                                <th scope="col">DNS</th>
                                <th scope="col"><span class="sr-only">Actions</span></th>
                            </tr>
                        </thead>
                        <tbody>
                            <template v-for="ip in data.ips" :key="ip.id">
                                <tr :data-ip="ip.name">
                                    <th scope="row" class="nowrap">
                                        {{ ip.name }}
                                        <span v-if="ip.is_primary" class="tag" data-testid="main-tag">Main IP</span>
                                    </th>
                                    <td class="nowrap"><code>{{ ip.address }}</code></td>
                                    <td class="nowrap"><code>{{ ip.helo_name }}</code></td>
                                    <td class="nowrap"><code>{{ ip.bounce_domain }}</code></td>
                                    <td class="num" :title="ip.is_primary ? `${ip.app_count} chosen it, ${data.unpinned_app_count} haven't chosen an IP` : undefined">{{ formatCount(appsUsing(ip, data.unpinned_app_count)) }}</td>
                                    <td class="nowrap">
                                        <span class="tag" :class="workerTag(ip)" data-testid="on-worker" :title="workerTitle(ip)">{{ workerText(ip) }}</span>
                                    </td>
                                    <td>
                                        <div class="dns-cell">
                                            <button type="button" class="btn sm" :disabled="checkingId === ip.id" data-testid="check-dns" @click="checkDns(ip)">
                                                {{ checkingId === ip.id ? 'Checking…' : checks[ip.id] ? 'Check again' : 'Check DNS' }}<span class="sr-only"> for {{ ip.name }}</span>
                                            </button>
                                            <span v-if="checks[ip.id]" class="tag" :class="dnsStatus(summary(checks[ip.id]!)).tone" data-testid="dns-summary">{{ summaryText(checks[ip.id]!) }}</span>
                                        </div>
                                    </td>
                                    <td class="row-actions">
                                        <div class="row-buttons">
                                            <button type="button" class="btn sm" :disabled="!!blocker || isSaving" :title="blocker ?? undefined" data-testid="edit-ip" @click="openEdit(ip)">
                                                <i class="fa-solid fa-pen" aria-hidden="true" />Edit<span class="sr-only"> {{ ip.name }}</span>
                                            </button>
                                            <button type="button" class="btn sm danger" :disabled="!!deleteBlock(ip) || isSaving" :aria-describedby="deleteBlock(ip) ? `why-${ip.id}` : undefined" data-testid="delete-ip" @click="remove(ip)">
                                                <i class="fa-solid fa-trash" aria-hidden="true" />Delete<span class="sr-only"> {{ ip.name }}</span>
                                            </button>
                                        </div>
                                    </td>
                                </tr>
                                <tr v-if="workerNote(ip) || deleteBlock(ip) || checks[ip.id]" class="detail" :data-detail="ip.name">
                                    <td colspan="8">
                                        <p v-if="workerNote(ip)" class="note warn" data-testid="worker-note"><i class="fa-solid fa-triangle-exclamation" aria-hidden="true" /> {{ workerNote(ip) }}</p>
                                        <p v-if="deleteBlock(ip) && !blocker" :id="`why-${ip.id}`" class="note" data-testid="delete-reason">Can't delete: {{ deleteBlock(ip) }}</p>
                                        <ul v-if="checks[ip.id]" class="checks" data-testid="dns-results">
                                            <li v-for="check in DNS_CHECKS" :key="check.key">
                                                <span class="tag" :class="dnsStatus(checks[ip.id]![check.key]).tone">{{ dnsStatus(checks[ip.id]![check.key]).text }}</span>
                                                {{ check.label }}
                                                <template v-if="check.key === 'reverse' && checks[ip.id]!.reverse_names.length"> (currently: <code>{{ checks[ip.id]!.reverse_names.join(', ') }}</code>)</template>
                                            </li>
                                            <li v-if="checks[ip.id]!.bounce_spf === 'mismatch'" class="hint">{{ BOUNCE_SPF_CAVEAT }}</li>
                                        </ul>
                                        <p v-if="checkErrors[ip.id]" class="note warn">{{ checkErrors[ip.id]!.message }}</p>
                                    </td>
                                </tr>
                            </template>
                            <tr v-for="address in data.unnamed_edge_addresses" :key="address" class="unnamed" data-testid="unnamed">
                                <th scope="row" class="nowrap muted">
                                    Not named yet
                                    <span v-if="address === data.edge_primary_address" class="tag">Main IP</span>
                                </th>
                                <td class="nowrap"><code>{{ address }}</code></td>
                                <td class="muted" colspan="2">{{ address === data.edge_primary_address ? 'Uses the sending worker\'s own HELO name and the standard bounce domain.' : 'The sending worker has this address, but no app can use it until you name it.' }}</td>
                                <td class="num">{{ address === data.edge_primary_address ? formatCount(data.unpinned_app_count) : '0' }}</td>
                                <td class="nowrap"><span class="tag success">Yes</span></td>
                                <td />
                                <td class="row-actions">
                                    <div class="row-buttons">
                                        <button type="button" class="btn sm" :disabled="!!blocker || isSaving" :title="blocker ?? undefined" data-testid="name-ip" @click="openAdd(address)">
                                            Name it<span class="sr-only"> {{ address }}</span>
                                        </button>
                                    </div>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>

                <div v-if="groups.length" class="spf" data-testid="spf">
                    <h3>SPF records</h3>
                    <p class="hint">Receiving mail servers check that mail sent for a bounce domain comes from an address its SPF record lists. Add these entries to each domain's SPF record (a DNS TXT record that starts with <code>v=spf1</code>), keeping whatever it already lists.</p>
                    <ul>
                        <li v-for="group in groups" :key="group.domain" :data-spf-domain="group.domain">
                            <code class="domain">{{ group.domain }}</code>
                            <code class="entries" :title="`${group.addresses.length} ${group.addresses.length === 1 ? 'address' : 'addresses'}`">{{ group.entries }}</code>
                        </li>
                    </ul>
                </div>
            </template>
        </div>

        <SendingIpModal
            v-if="modal && data"
            :ip="modal.ip"
            :locked-address="modal.address"
            :is-submitting="isSaving"
            :error="saveError"
            @close="closeModal"
            @create="create"
            @update="update"
        />
    </section>
</template>

<script setup lang="ts">
import { showConfirmDestroy, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, onMounted, reactive, ref, shallowRef } from 'vue';

import { describeApiError, type ApiErrorDescription } from '@/api-errors';
import { authState } from '@/auth';
import { formatDateTime } from '@/format';
import { DefaultApi, type SendingIp, type SendingIpDnsCheck, type SendingIps } from '@/openapi-client-generated';
import {
    appsUsing,
    BOUNCE_SPF_CAVEAT,
    changeBlocker,
    deleteBlocker,
    DNS_CHECKS,
    dnsStatus,
    onWorker,
    parseDnsCheck,
    parseSendingIp,
    parseSendingIps,
    spfGroups,
    workerNote
} from '@/sending-ips';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import SendingIpModal from '@/shared/components/sending-ip-modal.vue';
import { formatCount } from '@/submission-rate';

const props = defineProps<{
    /** Mail intake mode from the operations snapshot; changes need it open. Null when unknown. */
    gateMode: string | null;
}>();

const data = shallowRef<SendingIps | null>(null);
const isLoading = ref(false);
const isSaving = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const actionError = ref<ApiErrorDescription | null>(null);
const saveError = ref<ApiErrorDescription | null>(null);
const modal = ref<{ ip?: SendingIp; address?: string } | null>(null);
const checkingId = ref<string | null>(null);
const checks = reactive<Record<string, SendingIpDnsCheck | undefined>>({});
const checkErrors = reactive<Record<string, ApiErrorDescription | undefined>>({});

let seq = 0;
let isUnmounted = false;

const signedIn = computed(() => authState.status === 'signed-in');
const blocker = computed(() => changeBlocker(props.gateMode, signedIn.value));
const groups = computed(() => spfGroups(data.value?.ips ?? []));

const deleteBlock = (ip: SendingIp) => deleteBlocker(ip, props.gateMode, signedIn.value);

function workerText(ip: SendingIp) {
    return { yes: 'Yes', no: 'No', unknown: 'Unknown' }[onWorker(ip)];
}
function workerTag(ip: SendingIp) {
    return { yes: ip.bounce_on_edge === false ? 'warning' : 'success', no: 'warning', unknown: '' }[onWorker(ip)];
}
function workerTitle(ip: SendingIp) {
    return {
        yes: 'The sending worker reported this address at its last start.',
        no: 'The sending worker did not report this address.',
        unknown: 'The sending worker hasn\'t reported its addresses, so this isn\'t known.'
    }[onWorker(ip)];
}

/** The worst of the four checks, for the one-word result next to the button. */
function summary(check: SendingIpDnsCheck): string {
    const values = DNS_CHECKS.map(c => check[c.key]);
    for (const bad of ['mismatch', 'missing', 'unavailable']) if (values.includes(bad as never)) return bad;
    return 'ok';
}
function summaryText(check: SendingIpDnsCheck): string {
    const s = summary(check);
    return s === 'ok' ? 'All good' : s === 'unavailable' ? 'Couldn\'t check everything' : 'Needs attention';
}

async function load() {
    const mine = ++seq;
    isLoading.value = true;
    loadError.value = null;
    try {
        const { data: body } = await DefaultApi.listSendingIps();
        if (isUnmounted || mine !== seq) return;
        data.value = parseSendingIps(body);
    } catch (err) {
        if (isUnmounted || mine !== seq) return;
        data.value = null;
        loadError.value = describeApiError(err, { unavailable: 'Sending IPs are temporarily unavailable. Try again in a moment.' });
    } finally {
        if (mine === seq) isLoading.value = false;
    }
}

function openAdd(address?: string) {
    if (blocker.value) return;
    saveError.value = null;
    modal.value = { address };
}
function openEdit(ip: SendingIp) {
    if (blocker.value) return;
    saveError.value = null;
    modal.value = { ip };
}
function closeModal() {
    if (!isSaving.value) modal.value = null;
}

const MUTATION_ERRORS = {
    mutation: true,
    notFound: 'That sending IP was not found. It may have been removed. Reload to check.',
    unavailable: 'Changing sending IPs is unavailable right now. Mail intake may be closed. Try again in a moment.'
} as const;

async function create(input: { name: string; address: string; helo_name: string; bounce_domain: string }) {
    if (blocker.value || isSaving.value) return;
    isSaving.value = true;
    saveError.value = null;
    seq++;
    isLoading.value = false;
    let saved = false;
    try {
        const { data: body } = await DefaultApi.createSendingIp({ body: input });
        parseSendingIp(body);
        saved = true;
    } catch (err) {
        if (!isUnmounted) {
            saveError.value = describeApiError(err, { ...MUTATION_ERRORS, conflict: 'Another sending IP already has that name or address, or the limit of 64 IPs is reached. Nothing was changed.' });
        }
    } finally {
        isSaving.value = false;
    }
    if (isUnmounted) return;
    if (saved) {
        modal.value = null;
        showToast({ message: `Added “${input.name}”. It can be used once the sending worker has the address.`, durationSecs: 6 });
    }
    await load();
}

async function update(patch: { name?: string; helo_name?: string; bounce_domain?: string }) {
    const target = modal.value?.ip;
    if (!target || blocker.value || isSaving.value) return;
    isSaving.value = true;
    saveError.value = null;
    seq++;
    isLoading.value = false;
    let saved = false;
    try {
        const { data: body } = await DefaultApi.updateSendingIp({ path: { sendingIpId: target.id }, body: patch });
        parseSendingIp(body);
        saved = true;
    } catch (err) {
        if (!isUnmounted) {
            saveError.value = describeApiError(err, { ...MUTATION_ERRORS, conflict: 'Another sending IP already has that name. Nothing was changed.' });
        }
    } finally {
        isSaving.value = false;
    }
    if (isUnmounted) return;
    if (saved) {
        modal.value = null;
        delete checks[target.id];
        showToast({ message: `Saved “${patch.name ?? target.name}”. The change applies to mail sent from now on.`, durationSecs: 6 });
    }
    await load();
}

async function remove(ip: SendingIp) {
    if (deleteBlock(ip) || isSaving.value) return;
    const ok = await showConfirmDestroy(
        `Delete ${ip.name}?`,
        ip.is_primary
            ? 'The name and its HELO and bounce settings are removed. Apps that haven\'t chosen an IP will then send with the sending worker\'s own hostname and the default bounce domain. Mail already sent keeps its history. The address stays available to name again.'
            : 'The name and its HELO and bounce settings are removed. Mail already sent keeps its history. If this address is still set up on the sending worker, it stays available to name again.'
    );
    if (!ok || isUnmounted) return;
    isSaving.value = true;
    actionError.value = null;
    seq++;
    isLoading.value = false;
    try {
        await DefaultApi.deleteSendingIp({ path: { sendingIpId: ip.id } });
        showToast({ message: `Deleted “${ip.name}”.`, durationSecs: 4 });
    } catch (err) {
        if (!isUnmounted) {
            actionError.value = describeApiError(err, { ...MUTATION_ERRORS, conflict: 'An app uses this IP, so it can\'t be deleted. Move the app to another IP first.' });
        }
    } finally {
        isSaving.value = false;
    }
    if (!isUnmounted) await load();
}

async function checkDns(ip: SendingIp) {
    if (checkingId.value) return;
    checkingId.value = ip.id;
    checkErrors[ip.id] = undefined;
    try {
        const { data: body } = await DefaultApi.checkSendingIpDns({ path: { sendingIpId: ip.id } });
        if (isUnmounted) return;
        checks[ip.id] = parseDnsCheck(body);
    } catch (err) {
        if (isUnmounted) return;
        checks[ip.id] = undefined;
        checkErrors[ip.id] = describeApiError(err, { notFound: 'That sending IP was not found. Reload to check.', unavailable: 'The DNS check is unavailable right now. Try again in a moment.' });
    } finally {
        if (!isUnmounted) checkingId.value = null;
    }
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

.hint {
    color: var(--text-3);
    font-size: 12px;
}

.nowrap {
    white-space: nowrap;
}

.card-header .actions {
    display: flex;
    gap: 6px;
    justify-content: flex-end;
}

// The cell stays a table cell (so its height and border match the row); the
// buttons are laid out inside it.
.row-actions {
    text-align: right;
    vertical-align: middle;
}

.row-buttons {
    display: inline-flex;
    gap: 6px;
    white-space: nowrap;
}

.dns-cell {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 4px 6px;
}

.detail td {
    padding-top: 0;
}

.note {
    margin: 2px 0;
    font-size: 12px;
    color: var(--text-3);

    &.warn {
        color: var(--warning, var(--text-2));
    }
}

.checks {
    margin: 4px 0 0;
    padding: 0;
    list-style: none;
    display: flex;
    flex-direction: column;
    gap: 4px;
    font-size: 12px;
}

.spf {
    display: flex;
    flex-direction: column;
    gap: 6px;

    h3 {
        margin: 0;
        font-size: 13px;
    }

    ul {
        margin: 0;
        padding: 0;
        list-style: none;
        display: flex;
        flex-direction: column;
        gap: 4px;
    }

    li {
        display: flex;
        flex-wrap: wrap;
        gap: 4px 12px;
        align-items: baseline;
    }

    .domain {
        font-weight: 600;
    }

    .entries {
        overflow-wrap: anywhere;
    }
}

.unnamed td,
.unnamed th {
    background: transparent;
}
</style>
