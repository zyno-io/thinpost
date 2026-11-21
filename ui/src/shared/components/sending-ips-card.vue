<template>
    <section class="card" aria-labelledby="sending-ips-heading" :aria-busy="isLoading">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 id="sending-ips-heading">Sending IPs</h2>
                <HelpButton topic="sending IPs">
                    <p>Mail leaves Thinpost from an IP address. Giving different apps different IPs keeps one app's reputation with mailbox providers from affecting another's.</p>
                    <ul>
                        <li>The <strong>main IP</strong> is the sending worker's first address. Every app uses it unless you choose another IP for it.</li>
                        <li>The IPs and their names are set up on the sending worker and appear here when it connects. To add or change one, ask your Thinpost administrator.</li>
                        <li>If an app uses an IP the sending worker no longer has, its mail waits. It is never sent from a different address. Move those apps to another IP.</li>
                        <li>Changes to which IP an app uses apply to mail sent from now on. A delivery already with the sending worker finishes from the address it started with.</li>
                        <li>Warm up a new IP by moving one low-volume app to it first.</li>
                    </ul>
                    <p>For each IP, whoever manages your DNS should set its reverse DNS to the HELO name, point the HELO name at the IP, give the HELO name an MX record so bounces come back, and list the IP in the HELO name's SPF record. The DNS check reads public DNS and doesn't change anything.</p>
                </HelpButton>
            </div>
            <div class="actions">
                <button type="button" class="btn sm" :disabled="isLoading" @click="load">
                    <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isLoading && !!data }" aria-hidden="true" />Refresh
                </button>
            </div>
        </div>
        <div class="card-body rows">
            <Loader v-if="isLoading && !data" label="Loading sending IPs" />
            <ApiErrorNotice v-if="loadError" :error="loadError" :retry="load" retry-label="Reload" />
            <template v-if="data">
                <p class="hint" data-testid="read-only-note">These IPs and their names are set up on the sending worker and appear here when it connects. To add or change one, ask your Thinpost administrator.</p>
                <p v-if="!data.edge_reported_at" class="hint" data-testid="no-report">The sending worker hasn't connected yet, so its IPs aren't known.</p>
                <p v-else-if="!data.edge_reports_identity" class="hint" data-testid="no-sources">The sending worker didn't report any IPs with names (it may be an older version), so it isn't sending mail. Update it to a version that reports them.</p>
                <p v-else class="hint" data-testid="reported-at">The sending worker last reported its IPs <time :datetime="data.edge_reported_at">{{ formatDateTime(data.edge_reported_at) }}</time>.</p>

                <p v-if="!data.ips.length" class="muted" data-testid="empty">No sending IPs are known yet. Every app sends from the main IP once the sending worker connects.</p>

                <div v-else class="table-wrap">
                    <table class="data compact" data-testid="sending-ips">
                        <caption class="sr-only">Sending IPs</caption>
                        <thead>
                            <tr>
                                <th scope="col">IP address</th>
                                <th scope="col">HELO name</th>
                                <th scope="col" class="num">Apps using it</th>
                                <th scope="col">On the sending worker</th>
                                <th scope="col">DNS</th>
                            </tr>
                        </thead>
                        <tbody>
                            <template v-for="ip in data.ips" :key="ip.id">
                                <tr :data-ip="ip.address">
                                    <th scope="row" class="nowrap">
                                        <code>{{ ip.address }}</code>
                                        <span v-if="ip.is_primary" class="tag" data-testid="main-tag">Main IP</span>
                                    </th>
                                    <td class="nowrap"><code>{{ ip.helo_name }}</code></td>
                                    <td class="num" :title="ip.is_primary ? `${ip.app_count} chose it, ${data.unpinned_app_count} haven't chosen an IP` : undefined">{{ formatCount(appsUsing(ip, data.unpinned_app_count)) }}</td>
                                    <td class="nowrap">
                                        <span class="tag" :class="workerTag(ip)" data-testid="on-worker" :title="workerTitle(ip)">{{ workerText(ip) }}</span>
                                    </td>
                                    <td>
                                        <div class="dns-cell">
                                            <button type="button" class="btn sm" :disabled="checkingId === ip.id" data-testid="check-dns" @click="checkDns(ip)">
                                                {{ checkingId === ip.id ? 'Checking…' : checks[ip.id] ? 'Check again' : 'Check DNS' }}<span class="sr-only"> for {{ ip.helo_name }}</span>
                                            </button>
                                            <span v-if="checks[ip.id]" class="tag" :class="dnsStatus(summary(checks[ip.id]!)).tone" data-testid="dns-summary">{{ summaryText(checks[ip.id]!) }}</span>
                                        </div>
                                    </td>
                                </tr>
                                <tr v-if="workerNote(ip) || checks[ip.id] || checkErrors[ip.id]" class="detail" :data-detail="ip.address">
                                    <td colspan="5">
                                        <p v-if="workerNote(ip)" class="note warn" data-testid="worker-note"><i class="fa-solid fa-triangle-exclamation" aria-hidden="true" /> {{ workerNote(ip) }}</p>
                                        <ul v-if="checks[ip.id]" class="checks" data-testid="dns-results">
                                            <li v-for="check in DNS_CHECKS" :key="check.key">
                                                <span class="tag" :class="dnsStatus(checks[ip.id]![check.key]).tone">{{ dnsStatus(checks[ip.id]![check.key]).text }}</span>
                                                {{ check.label }}
                                                <template v-if="check.key === 'reverse' && checks[ip.id]!.reverse_names.length"> (currently: <code>{{ checks[ip.id]!.reverse_names.join(', ') }}</code>)</template>
                                            </li>
                                            <li v-if="checks[ip.id]!.spf === 'mismatch'" class="hint">{{ SPF_CAVEAT }}</li>
                                        </ul>
                                        <div v-if="checks[ip.id] && hints(ip).length" class="dns-hints" data-testid="dns-hints">
                                            <p class="hint">To fix it, ask whoever manages your DNS to:</p>
                                            <ul>
                                                <li v-for="hint in hints(ip)" :key="hint">{{ hint }}</li>
                                            </ul>
                                        </div>
                                        <p v-if="checkErrors[ip.id]" class="note warn">{{ checkErrors[ip.id]!.message }}</p>
                                    </td>
                                </tr>
                            </template>
                        </tbody>
                    </table>
                </div>
            </template>
        </div>
    </section>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, reactive, ref, shallowRef } from 'vue';

import { describeApiError, type ApiErrorDescription } from '@/api-errors';
import { formatDateTime } from '@/format';
import { DefaultApi, type SendingIp, type SendingIpDnsCheck, type SendingIps } from '@/openapi-client-generated';
import { appsUsing, DNS_CHECKS, dnsHints, dnsStatus, onWorker, parseDnsCheck, parseSendingIps, SPF_CAVEAT, workerNote } from '@/sending-ips';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import { formatCount } from '@/submission-rate';

const data = shallowRef<SendingIps | null>(null);
const isLoading = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const checkingId = ref<string | null>(null);
const checks = reactive<Record<string, SendingIpDnsCheck | undefined>>({});
const checkErrors = reactive<Record<string, ApiErrorDescription | undefined>>({});

let seq = 0;
let isUnmounted = false;

function workerText(ip: SendingIp) {
    return { yes: 'Yes', no: 'No', unknown: 'Unknown' }[onWorker(ip)];
}
function workerTag(ip: SendingIp) {
    return { yes: 'success', no: 'warning', unknown: '' }[onWorker(ip)];
}
function workerTitle(ip: SendingIp) {
    return {
        yes: 'The sending worker reported this IP when it last connected.',
        no: 'The sending worker did not report this IP when it last connected.',
        unknown: 'The sending worker hasn\'t reported its IPs, so this isn\'t known.'
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
function hints(ip: SendingIp): string[] {
    const check = checks[ip.id];
    return check ? dnsHints(ip, check) : [];
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

.dns-hints {
    margin-top: 6px;
    font-size: 12px;

    ul {
        margin: 2px 0 0;
        padding-left: 18px;
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
</style>
