<template>
    <div id="app-domains">
        <Loader v-if="isLoading" label="Loading sending domains" />

        <ApiErrorNotice v-else-if="loadError" :error="loadError" :retry="load" />

        <section v-else aria-labelledby="domains-heading" class="card">
            <div class="card-header">
                <div class="heading-with-help">
                    <h2 id="domains-heading">
                        Sending domains
                    </h2>
                    <HelpButton topic="sending domains">
                        <p>
                            <strong>Verified means Thinpost can see your DKIM record, so mail from the domain can be signed.</strong> Adding a domain
                            gives you a DKIM record to put in your DNS. Once Thinpost finds it, the domain is verified and mail from it is signed
                            with that key, so DKIM passes and lines up with DMARC.
                        </p>
                        <p>
                            Verifying doesn't set up or check SPF, DMARC or reverse DNS, so it doesn't promise mail will reach inboxes. To change a
                            domain's key, choose Rotate key: add the new record first, then switch, so there's no gap. Domains can't be removed here.
                        </p>
                        <p>
                            Each verified domain can also have its own <strong>bounce domain</strong>, so bounces come back to a name of yours and SPF passes for your domain too.
                            It's optional; without it, Thinpost's standard bounce address is used.
                        </p>
                        <p>
                            Where tracking is available, a verified domain can also have its own <strong>tracking domain</strong>, so links and the open-tracking image in your emails show your domain.
                        </p>
                    </HelpButton>
                </div>
                <button type="button" class="btn primary" :disabled="!canRequest" :aria-describedby="app.active ? undefined : 'request-hint'" @click="openRequest">
                    <i class="fa-solid fa-plus" aria-hidden="true" />
                    Add domain
                </button>
            </div>

            <div class="card-body domains-body">
                <p v-if="!app.active" id="request-hint" class="hint">This app is paused, so new DKIM records can't be requested.</p>
                <p v-if="testSend.blocker.value && testSend.blocker.value !== pausedHint" class="hint">{{ testSend.blocker.value }}</p>
                <ApiErrorNotice v-if="testSend.outcomeError.value" :error="testSend.outcomeError.value" />

                <ApiErrorNotice v-if="actionError" :error="actionError" />

                <Loader v-if="isReloading && !domains" label="Loading sending domains" />
                <ApiErrorNotice v-else-if="domainsError" :error="domainsError" :retry="reloadDomains" />

                <div v-else-if="domains && !domains.length" class="empty">
                    <i class="fa-solid fa-globe" aria-hidden="true" />
                    <h2>No sending domains yet</h2>
                </div>

                <div v-if="recordDomains.length" class="records" aria-label="DKIM records to publish">
                    <article v-for="domain in recordDomains" :key="domain.name" class="card record" :aria-labelledby="`record-${domain.name}`">
                        <div class="record-header">
                            <div class="heading-with-help">
                                <h3 :id="`record-${domain.name}`">
                                    {{ recordTitle(domain) }}
                                </h3>
                                <HelpButton topic="DKIM records">
                                    <p>
                                        This is a TXT record at your DNS provider. Thinpost made the key, and the record holds only its public half. Keep exactly
                                        one record at this name. It stays here for you to copy until the domain is set up.
                                    </p>
                                </HelpButton>
                            </div>
                            <button v-if="domain.active_key && !domain.pending_key" type="button" class="btn sm" @click="hideRecord(domain.name)">
                                Hide
                                <span class="sr-only">DKIM record for {{ domain.name }}</span>
                            </button>
                        </div>
                        <p class="hint">{{ recordHint(domain) }}</p>
                        <DkimRecord :name="shownKey(domain)!.dnsName" :value="shownKey(domain)!.dnsValue" />
                    </article>
                </div>
            </div>

            <div v-if="domains && domains.length && !domainsError" class="table-wrap" :aria-busy="isReloading">
                <table class="data">
                    <caption class="sr-only">Sending domains for {{ app.name }}</caption>
                    <thead>
                        <tr>
                            <th scope="col">Domain</th>
                            <th scope="col">Status</th>
                            <th scope="col">DKIM key</th>
                            <th scope="col">Added</th>
                            <th scope="col">Verified</th>
                            <th scope="col"><span class="sr-only">Actions</span></th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr v-for="domain in domains" :key="domain.name">
                            <th scope="row" class="name">{{ domain.name }}</th>
                            <td>
                                <span class="tag" :class="statusClass(domain)">{{ statusLabel(domain) }}</span>
                            </td>
                            <td class="key-cell">
                                <template v-if="domain.active_key">
                                    <code>{{ domain.active_key.selector }}</code> <span class="muted">in use</span>
                                </template>
                                <span v-else-if="!domain.pending_key" class="muted">No DKIM key yet</span>
                                <p v-if="domain.pending_key" class="key-note">{{ pendingNote(domain) }}</p>
                                <p v-if="domain.retired_key" class="key-note">
                                    Old record <code>{{ domain.retired_key.selector }}</code> can stay in DNS until
                                    <time :datetime="domain.retired_key.keep_record_until">{{ formatDateTime(domain.retired_key.keep_record_until) }}</time>.
                                </p>
                            </td>
                            <td>
                                <time :datetime="domain.created_at">{{ formatDateTime(domain.created_at) }}</time>
                            </td>
                            <td>
                                <time v-if="domain.verified_at" :datetime="domain.verified_at">{{ formatDateTime(domain.verified_at) }}</time>
                                <span v-else class="muted">—</span>
                            </td>
                            <td class="actions">
                                <button
                                    v-if="domain.active && domain.active_key"
                                    type="button"
                                    class="btn sm"
                                    :disabled="!!testSend.blocker.value || !canAct"
                                    @click="testSend.open(domain.name)"
                                >
                                    Send test
                                    <span class="sr-only">from {{ domain.name }}</span>
                                </button>
                                <template v-if="domain.pending_key">
                                    <button type="button" class="btn sm" :disabled="!canAct" @click="verify(domain)">
                                        {{ isBusy(domain.name, 'verify') ? 'Checking…' : domain.active_key ? 'Check record' : 'Verify' }}
                                        <span class="sr-only">{{ domain.name }}</span>
                                    </button>
                                    <button v-if="domain.active_key" type="button" class="btn sm" :class="{ primary: domain.pending_key.published_at }" :disabled="!canAct" @click="switchKey(domain)">
                                        {{ isBusy(domain.name, 'switch') ? 'Switching…' : 'Switch' }}
                                        <span class="sr-only">to the new key for {{ domain.name }}</span>
                                    </button>
                                    <button v-if="domain.active_key" type="button" class="btn sm" :disabled="!canAct" @click="cancelKey(domain)">
                                        {{ isBusy(domain.name, 'cancel') ? 'Cancelling…' : 'Cancel' }}
                                        <span class="sr-only">the new key for {{ domain.name }}</span>
                                    </button>
                                </template>
                                <template v-else-if="domain.active_key">
                                    <button type="button" class="btn sm" @click="toggleRecord(domain.name)">
                                        {{ shown.has(domain.name) ? 'Hide record' : 'Show record' }}
                                        <span class="sr-only">for {{ domain.name }}</span>
                                    </button>
                                    <button type="button" class="btn sm" :disabled="!canAct || !app.active" @click="createKey(domain)">
                                        {{ isBusy(domain.name, 'create') ? 'Working…' : 'Rotate key' }}
                                        <span class="sr-only">for {{ domain.name }}</span>
                                    </button>
                                </template>
                                <button v-else type="button" class="btn sm primary" :disabled="!canAct || !app.active" @click="createKey(domain)">
                                    {{ isBusy(domain.name, 'create') ? 'Working…' : 'Create DKIM key' }}
                                    <span class="sr-only">for {{ domain.name }}</span>
                                </button>
                            </td>
                        </tr>
                    </tbody>
                </table>
            </div>

            <div v-if="verifiedDomains.length && !domainsError" class="card-body bounce-panels" role="group" aria-label="Bounce domains">
                <BounceDomainPanel v-for="domain in verifiedDomains" :key="domain.name" :app-id="app.id" :app-active="app.active" :can-act="canAct" :domain="domain" @changed="reloadDomains" />
            </div>

            <div v-if="trackingPanelDomains.length && !domainsError" class="card-body bounce-panels" role="group" aria-label="Tracking domains">
                <TrackingDomainPanel
                    v-for="domain in trackingPanelDomains"
                    :key="domain.name"
                    :app-id="app.id"
                    :app-active="app.active"
                    :can-act="canAct"
                    :domain="domain"
                    :support="trackingSupport"
                    @changed="reloadDomains"
                />
            </div>
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
        <DomainModal v-if="isRequestOpen" :is-submitting="isRequesting" :error="requestError" @close="closeRequest" @submit="requestDomain" />
    </div>
</template>

<script lang="ts" setup>
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';

import { apiErrorReason, apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { parseDkimKey, type DkimKeyView } from '@/dkim';
import { formatDateTime } from '@/format';
import { DefaultApi, type Domain } from '@/openapi-client-generated';
import { loadRuntimeFeatures, runtimeTracking } from '@/runtime-features';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import BounceDomainPanel from '@/shared/components/bounce-domain-panel.vue';
import DkimRecord from '@/shared/components/dkim-record.vue';
import HelpButton from '@/shared/components/help-button.vue';
import DomainModal from '@/shared/components/domain-modal.vue';
import Loader from '@/shared/components/loader.vue';
import TestMessageModal from '@/shared/components/test-message-modal.vue';
import TrackingDomainPanel from '@/shared/components/tracking-domain-panel.vue';
import { useTestSend } from '@/test-send';

type Action = 'verify' | 'switch' | 'cancel' | 'create';

const { app, appId } = useAppContext();
const testSend = useTestSend({ appId, isActive: computed(() => app.value.active) });
const pausedHint = 'This app is paused. Resume the app to send.';

const domains = shallowRef<Domain[] | null>(null);
const isLoading = ref(true);
const loadError = ref<ApiErrorDescription | null>(null);
const domainsError = ref<ApiErrorDescription | null>(null);
const actionError = ref<ApiErrorDescription | null>(null);
const isReloading = ref(false);

const isRequestOpen = ref(false);
const isRequesting = ref(false);
const requestError = ref<ApiErrorDescription | null>(null);
const busy = ref<{ domain: string; action: Action } | null>(null);

/** Domains whose current (active) record the person asked to see. A pending record is always shown. */
const shown = ref<Set<string>>(new Set());

const isSignedIn = computed(() => authState.status === 'signed-in');
const canRequest = computed(() => app.value.active && isSignedIn.value && !isRequesting.value && !busy.value);
const canAct = computed(() => isSignedIn.value && !busy.value && !isRequesting.value);

/** Verified domains each get a bounce-domain panel. */
const verifiedDomains = computed(() => (domains.value ?? []).filter(d => d.active));

/** What the deployment offers for custom tracking addresses; unknown until the runtime report loads. */
const trackingSupport = computed(() => ({ available: runtimeTracking.value?.custom_domains === true, cnameTarget: runtimeTracking.value?.cname_target || null }));
/** Tracking panels appear only where tracking exists: for every verified domain when apps may add their own address, else only for one that already has it. */
const trackingPanelDomains = computed(() => (runtimeTracking.value ? verifiedDomains.value.filter(d => trackingSupport.value.available || d.tracking_domain) : []));
void loadRuntimeFeatures();

/** Domains with a record card: a pending key always, an active key when asked for. */
const recordDomains = computed(() => (domains.value ?? []).filter(d => shownKey(d) !== null));

let loadSeq = 0;
let reloadSeq = 0;
let isUnmounted = false;
onBeforeUnmount(() => {
    isUnmounted = true;
});

watch(appId, load, { immediate: true });

function shownKey(domain: Domain): DkimKeyView | null {
    const pending = parseDkimKey(domain.pending_key);
    if (pending) return pending;
    return shown.value.has(domain.name) ? parseDkimKey(domain.active_key) : null;
}

function toggleRecord(name: string) {
    const next = new Set(shown.value);
    if (!next.delete(name)) next.add(name);
    shown.value = next;
}

function hideRecord(name: string) {
    const next = new Set(shown.value);
    next.delete(name);
    shown.value = next;
}

function recordTitle(domain: Domain) {
    if (domain.pending_key && domain.active_key) return `New DKIM record for ${domain.name}`;
    return `DKIM record for ${domain.name}`;
}

function recordHint(domain: Domain) {
    if (domain.pending_key && domain.active_key) {
        return `This is the next key for ${domain.name}. Add it as a new record (it has its own name, so the current one keeps working), then choose Check record. When Thinpost can see it, choose Switch.`;
    }
    if (domain.pending_key) {
        return `Add this TXT record at your DNS provider, then choose Verify. Mail from ${domain.name} is signed with this key, so DKIM passes and lines up with DMARC. Keep exactly one record at this name.`;
    }
    return `This record is published for ${domain.name}. Keep exactly one record at this name.`;
}

function pendingNote(domain: Domain) {
    const key = parseDkimKey(domain.pending_key);
    if (!key) return '';
    if (domain.active_key) {
        return key.published ? `New key ${key.selector}: record found, ready to switch.` : `New key ${key.selector}: waiting for its DNS record.`;
    }
    return `${key.selector}: waiting for its DNS record.`;
}

function statusLabel(domain: Domain) {
    if (domain.active) return domain.active_key ? 'Verified' : 'No DKIM key yet';
    return domain.pending_key ? 'Waiting for DNS record' : 'Not verified';
}

function statusClass(domain: Domain) {
    return domain.active && domain.active_key ? 'success' : 'warning';
}

function isBusy(domain: string, action: Action) {
    return busy.value?.domain === domain && busy.value.action === action;
}

async function load() {
    const id = appId.value;
    const seq = ++loadSeq;
    isLoading.value = true;
    loadError.value = null;
    domainsError.value = null;
    actionError.value = null;
    domains.value = null;
    shown.value = new Set();

    try {
        const response = await DefaultApi.listAppDomains({ path: { appId: id } });
        const list = response.data?.domains;
        if (seq !== loadSeq) return;
        if (!Array.isArray(list)) throw new InvalidResponseError();
        domains.value = list;
    } catch (err) {
        if (seq !== loadSeq) return;
        loadError.value = describeApiError(err, { notFound: 'The app was not found.' });
    } finally {
        if (seq === loadSeq) isLoading.value = false;
    }
}

async function reloadDomains() {
    const appId = app.value?.id;
    if (!appId) return;
    const seq = loadSeq;
    const thisReload = ++reloadSeq;
    const isCurrent = () => seq === loadSeq && thisReload === reloadSeq;
    isReloading.value = true;
    domainsError.value = null;
    try {
        const { data } = await DefaultApi.listAppDomains({ path: { appId } });
        if (!isCurrent()) return;
        if (!Array.isArray(data?.domains)) throw new InvalidResponseError();
        domains.value = data.domains;
    } catch (err) {
        if (!isCurrent()) return;
        domains.value = null;
        domainsError.value = describeApiError(err, { notFound: 'The app was not found.' });
    } finally {
        if (thisReload === reloadSeq) isReloading.value = false;
    }
}

function openRequest() {
    requestError.value = null;
    actionError.value = null;
    isRequestOpen.value = true;
}

function closeRequest() {
    if (isRequesting.value) return;
    isRequestOpen.value = false;
}

const KEY_ERRORS = {
    mutation: true,
    notFound: 'The app or domain was not found, or the app is paused.',
    conflict: 'This domain already has a DKIM key, or a new key is already waiting for its record. Reload to see where it stands.'
};

/** A key request answers with the record to publish; reject a response that lacks one. */
function checkRecordResponse(data: { dns_name?: unknown; dns_value?: unknown } | undefined) {
    if (!data || typeof data.dns_name !== 'string' || typeof data.dns_value !== 'string' || !data.dns_name || !data.dns_value) {
        throw new InvalidResponseError();
    }
}

async function requestDomain(domain: string) {
    const appId = app.value?.id;
    if (!appId || isRequesting.value) return;
    const seq = loadSeq;
    isRequesting.value = true;
    requestError.value = null;
    try {
        // Asking again for a domain still waiting for its record returns the same record.
        const { data } = await DefaultApi.requestAppDomain({ path: { appId }, body: { domain } });
        checkRecordResponse(data);
        if (isUnmounted || seq !== loadSeq) return;
        isRequestOpen.value = false;
        showToast({ message: `DKIM record ready for ${domain}`, durationSecs: 3 });
        void reloadDomains();
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        requestError.value = describeApiError(err, KEY_ERRORS);
    } finally {
        isRequesting.value = false;
    }
}

async function createKey(domain: Domain) {
    const appId = app.value?.id;
    if (!appId || busy.value) return;
    const seq = loadSeq;
    if (domain.active_key) {
        const ok = await showConfirm(
            'Rotate the DKIM key?',
            `Thinpost makes a new key for ${domain.name}. The current key keeps signing until you add the new record and choose Switch.`
        );
        if (!ok || isUnmounted || seq !== loadSeq || !canAct.value) return;
    }
    busy.value = { domain: domain.name, action: 'create' };
    actionError.value = null;
    try {
        const { data } = await DefaultApi.createAppDomainDkimKey({ path: { appId, domain: domain.name } });
        checkRecordResponse(data);
        if (isUnmounted || seq !== loadSeq) return;
        showToast({ message: `DKIM record ready for ${domain.name}`, durationSecs: 3 });
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        actionError.value = describeApiError(err, KEY_ERRORS);
    } finally {
        busy.value = null;
    }
    if (seq === loadSeq && !isUnmounted) await reloadDomains();
}

function describeCheckError(err: unknown, domain: Domain): ApiErrorDescription {
    const name = parseDkimKey(domain.pending_key)?.dnsName ?? `the DKIM record for ${domain.name}`;
    if (apiErrorStatus(err) === 503) {
        return { kind: 'unavailable', status: 503, message: `Thinpost couldn't look up DNS just now. Try again in a moment. If it keeps happening, tell your Thinpost administrator.` };
    }
    if (apiErrorStatus(err) === 409) {
        const messages: Record<string, string> = {
            not_found: `Thinpost can't see a TXT record at ${name} yet. DNS changes can take a while to spread, so try again later.`,
            multiple_records: `There is more than one TXT record at ${name}. Keep exactly one, the one shown here.`,
            different_key: `A TXT record exists at ${name}, but it isn't the key shown here. Check that you copied the whole value.`,
            testing_mode: `The record at ${name} is marked as a test (t=y). Remove that part and Thinpost can use it.`,
            unsupported: `The record at ${name} isn't a standard DKIM key record. Replace it with the value shown here.`
        };
        const reason = apiErrorReason(err);
        return {
            kind: 'conflict',
            status: 409,
            message: (reason && messages[reason]) || 'There is no new key waiting for a record. Reload to see where it stands.'
        };
    }
    return describeApiError(err, { mutation: true, notFound: `${domain.name} wasn't found for this app.` });
}

async function verify(domain: Domain) {
    const appId = app.value?.id;
    if (!appId || busy.value) return;
    const seq = loadSeq;
    busy.value = { domain: domain.name, action: 'verify' };
    actionError.value = null;
    try {
        const { data } = await DefaultApi.verifyAppDomain({ path: { appId, domain: domain.name } });
        if (isUnmounted || seq !== loadSeq) return;
        if (data?.active !== true) throw new InvalidResponseError();
        showToast({ message: domain.active_key ? `Thinpost can see the new record for ${domain.name}` : `${domain.name} verified`, durationSecs: 3 });
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        actionError.value = describeCheckError(err, domain);
    } finally {
        busy.value = null;
    }
    if (seq === loadSeq && !isUnmounted) await reloadDomains();
}

async function switchKey(domain: Domain) {
    const appId = app.value?.id;
    const pending = parseDkimKey(domain.pending_key);
    if (!appId || !pending || busy.value) return;
    const seq = loadSeq;
    const ok = await showConfirm(
        'Switch to the new key?',
        `Mail from ${domain.name} will be signed with ${pending.selector} from now on, and the current key is deleted for good. Keep the old DNS record for a week or more so mail still waiting to be delivered passes DKIM. This page shows the date after you switch.`
    );
    if (!ok || isUnmounted || seq !== loadSeq || !canAct.value) return;
    busy.value = { domain: domain.name, action: 'switch' };
    actionError.value = null;
    try {
        const { data } = await DefaultApi.activateAppDomainDkimKey({ path: { appId, domain: domain.name, selector: pending.selector } });
        if (isUnmounted || seq !== loadSeq) return;
        if (data?.active !== true) throw new InvalidResponseError();
        showToast({ message: `${domain.name} now signs with ${pending.selector}`, durationSecs: 3 });
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        actionError.value = describeCheckError(err, domain);
    } finally {
        busy.value = null;
    }
    if (seq === loadSeq && !isUnmounted) await reloadDomains();
}

async function cancelKey(domain: Domain) {
    const appId = app.value?.id;
    const pending = parseDkimKey(domain.pending_key);
    if (!appId || !pending || busy.value) return;
    const seq = loadSeq;
    const ok = await showConfirm(
        'Cancel the new key?',
        `Thinpost deletes the new key for ${domain.name} and ${domain.name} keeps signing with the current one. You can remove the new record from DNS.`
    );
    if (!ok || isUnmounted || seq !== loadSeq || !canAct.value) return;
    busy.value = { domain: domain.name, action: 'cancel' };
    actionError.value = null;
    try {
        await DefaultApi.cancelAppDomainDkimKey({ path: { appId, domain: domain.name, selector: pending.selector } });
        if (isUnmounted || seq !== loadSeq) return;
        showToast({ message: `New key for ${domain.name} cancelled`, durationSecs: 3 });
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        actionError.value = describeApiError(err, { mutation: true, notFound: 'That key is no longer waiting. Reload to see where it stands.' });
    } finally {
        busy.value = null;
    }
    if (seq === loadSeq && !isUnmounted) await reloadDomains();
}
</script>

<style lang="scss" scoped>
.bounce-panels {
    display: flex;
    flex-direction: column;
    gap: 12px;
    border-top: 1px solid var(--border);
}

.domains-body {
    display: flex;
    flex-direction: column;
    gap: 12px;
}

/* Hide the body when every child rendered nothing (only v-if comments remain). */
.domains-body.domains-body:not(:has(*)) {
    display: none;
}

.records {
    display: flex;
    flex-direction: column;
    gap: 12px;
}

.record {
    display: flex;
    flex-direction: column;
    gap: 12px;
    padding: 16px 18px;
    box-shadow: none;
}

.record-header {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 8px;

    h3 {
        font-size: 14px;
        font-weight: 600;
        overflow-wrap: anywhere;
    }
}

.name {
    font-weight: 600;
    overflow-wrap: anywhere;
}

.key-cell {
    .key-note {
        margin-top: 4px;
        color: var(--text-3);
        font-size: 12px;
    }
}

.actions {
    text-align: right;
    white-space: nowrap;

    .btn + .btn {
        margin-left: 8px;
    }
}
</style>
