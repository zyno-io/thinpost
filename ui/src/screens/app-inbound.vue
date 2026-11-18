<template>
    <div id="app-inbound">
        <section aria-labelledby="inbound-domains-heading" class="card" :aria-busy="isLoading">
            <div class="card-header">
                <div class="heading-with-help">
                    <h2 id="inbound-domains-heading">Receiving domains</h2>
                    <HelpButton topic="receiving domains">
                        <p>
                            A receiving domain is a domain whose incoming mail is meant for this application, for example a subdomain used for replies. To set
                            one up, add it here, publish the TXT record shown to prove you own it, then choose Verify. Once it is verified, point the domain's MX record at the Thinpost mail server shown with the records so incoming mail reaches Thinpost. Your application also needs an inbound mail webhook (add one on the Webhooks page): until it has one, senders keep trying for a few days.
                        </p>
                        <ul>
                            <li>Each domain belongs to one application. The first application to verify a domain owns it; others are told it is taken.</li>
                            <li>The MX check is a hint, not a requirement. It shows whether the domain's MX records name the Thinpost mail server.</li>
                            <li v-for="(mx, status) in MX_DESCRIPTIONS" :key="status">
                                <strong>{{ mx.label }}.</strong> {{ mx.detail }}
                            </li>
                            <li>
                                Use a subdomain you do not already use for mail. If the same domain has other MX records, only the mail sent to the Thinpost
                                server arrives here.
                            </li>
                            <li>Disabling a domain keeps it yours without receiving mail. Deleting it lets another application claim it.</li>
                        </ul>
                    </HelpButton>
                </div>
                <div class="header-actions">
                    <RouterLink class="btn" :to="addWebhookTarget" data-testid="add-inbound-webhook">
                        <i class="fa-solid fa-bolt" aria-hidden="true" />
                        Add inbound webhook
                    </RouterLink>
                    <button type="button" class="btn primary" :disabled="!canRequest" :aria-describedby="app.active ? undefined : 'inbound-request-hint'" @click="openAdd">
                        <i class="fa-solid fa-plus" aria-hidden="true" />
                        Add domain
                    </button>
                </div>
            </div>

            <div class="card-body inbound-body">
                <p v-if="!app.active" id="inbound-request-hint" class="hint">This application is paused, so new domains cannot be added.</p>
                <Loader v-if="isLoading" label="Loading receiving domains" />
                <ApiErrorNotice v-else-if="loadError" :error="loadError" :retry="load" />
                <ApiErrorNotice v-if="actionError" :error="actionError" />
                <div v-if="hasInboundWebhook === false" class="notice warning" data-testid="no-inbound-webhook">
                    <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                    <p class="notice-body">
                        <strong>No inbound webhook.</strong> Mail to your domains is deferred until you add one. Senders keep trying for a few days, and nothing is stored in the
                        meantime.
                        <RouterLink :to="addWebhookTarget" data-testid="add-inbound-webhook-warning">Add an inbound webhook</RouterLink>
                    </p>
                </div>
                <p v-if="domains && !mxHost" class="hint" data-testid="no-mx-host">
                    No receiving mail server is set up yet, so there is no MX record to show and the MX check is unavailable. Ask your Thinpost administrator.
                </p>
                <div v-if="domains && !domains.length && !loadError" class="empty">
                    <i class="fa-solid fa-inbox" aria-hidden="true" />
                    <h2>No receiving domains</h2>
                </div>
            </div>

            <div v-if="domains && domains.length" class="table-wrap">
                <table class="data">
                    <caption class="sr-only">Receiving domains for {{ app.name }}</caption>
                    <thead>
                        <tr>
                            <th scope="col">Domain</th>
                            <th scope="col">Status</th>
                            <th scope="col">MX record</th>
                            <th scope="col">Added</th>
                            <th scope="col"><span class="sr-only">Actions</span></th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr v-for="domain in domains" :key="domain.name">
                            <th scope="row" class="name">
                                {{ domain.name }}
                                <span v-if="domain.state === 'active' && mxHost" class="muted mx-note" data-testid="mx-instruction">
                                    Set its MX record to <code>{{ mxHost }}</code>
                                </span>
                            </th>
                            <td>
                                <span class="tag" :class="STATE_TONES[domain.state]">{{ STATE_LABELS[domain.state] }}</span>
                            </td>
                            <td>
                                <template v-if="domain.mx_status && domain.mx_checked_at">
                                    <span class="tag" :class="MX_DESCRIPTIONS[domain.mx_status].tone" :title="MX_DESCRIPTIONS[domain.mx_status].detail">
                                        {{ MX_DESCRIPTIONS[domain.mx_status].label }}
                                    </span>
                                    <span class="muted checked">
                                        checked <time :datetime="domain.mx_checked_at">{{ formatDateTime(domain.mx_checked_at) }}</time>
                                    </span>
                                </template>
                                <span v-else class="tag" title="Choose Check MX to look it up">Not checked</span>
                            </td>
                            <td>
                                <time :datetime="domain.created_at">{{ formatDateTime(domain.created_at) }}</time>
                            </td>
                            <td class="actions">
                                <button v-if="domain.state === 'pending'" type="button" class="btn sm" :disabled="!canAct" @click="verify(domain.name)">
                                    {{ isBusy(domain.name, 'verify') ? 'Verifying…' : 'Verify' }}
                                    <span class="sr-only">{{ domain.name }}</span>
                                </button>
                                <button v-if="domain.state === 'pending'" type="button" class="btn sm" :disabled="!canAct || !app.active" @click="newRecords(domain.name)">
                                    {{ isBusy(domain.name, 'records') ? 'Working…' : 'New DNS records' }}
                                    <span class="sr-only">for {{ domain.name }}</span>
                                </button>
                                <button type="button" class="btn sm" :disabled="!canAct || !mxHost" :title="mxHost ? undefined : 'No receiving mail server is set up yet'" @click="checkMx(domain.name)">
                                    {{ isBusy(domain.name, 'mx') ? 'Checking…' : 'Check MX' }}
                                    <span class="sr-only">{{ domain.name }}</span>
                                </button>
                                <button v-if="domain.state === 'active'" type="button" class="btn sm" :disabled="!canAct" @click="setEnabled(domain.name, false)">
                                    {{ isBusy(domain.name, 'disable') ? 'Disabling…' : 'Disable' }}
                                    <span class="sr-only">{{ domain.name }}</span>
                                </button>
                                <button v-if="domain.state === 'disabled'" type="button" class="btn sm" :disabled="!canAct" @click="setEnabled(domain.name, true)">
                                    {{ isBusy(domain.name, 'enable') ? 'Enabling…' : 'Enable' }}
                                    <span class="sr-only">{{ domain.name }}</span>
                                </button>
                                <button type="button" class="btn sm danger" :disabled="!canAct" @click="remove(domain.name)">
                                    {{ isBusy(domain.name, 'delete') ? 'Deleting…' : 'Delete' }}
                                    <span class="sr-only">{{ domain.name }}</span>
                                </button>
                            </td>
                        </tr>
                    </tbody>
                </table>
            </div>
        </section>

        <section aria-labelledby="inbound-limits-heading" class="card" :aria-busy="limitsLoading || isSaving">
            <div class="card-header">
                <div class="heading-with-help">
                    <h2 id="inbound-limits-heading">Limits</h2>
                    <HelpButton topic="inbound limits">
                        <p>These keep a flood of incoming mail from overwhelming your application. Mail over a limit is not lost: the sender is asked to try again later.</p>
                        <ul>
                            <li><strong>Messages per day</strong> is how many incoming messages are accepted each UTC day.</li>
                            <li><strong>Waiting for your webhook</strong> is how many accepted messages can be waiting to be handed to your webhook at once.</li>
                        </ul>
                    </HelpButton>
                </div>
                <button type="button" class="btn" :disabled="!limits || !isSignedIn || isSaving" @click="openLimits">
                    <i class="fa-solid fa-pen" aria-hidden="true" />
                    Edit limits
                </button>
            </div>
            <div class="card-body inbound-body">
                <Loader v-if="limitsLoading && !limits" label="Loading limits" />
                <ApiErrorNotice v-if="limitsError" :error="limitsError" :retry="loadLimits" />
                <dl v-if="limits" class="facts" data-testid="inbound-limits">
                    <div>
                        <dt>Messages per day</dt>
                        <dd>{{ formatCount(limits.daily_limit) }}</dd>
                    </div>
                    <div>
                        <dt>Waiting for your webhook</dt>
                        <dd>{{ formatCount(limits.pending_limit) }}</dd>
                    </div>
                </dl>
            </div>
        </section>

        <InboundMessageList :app-id="appId" :app-name="app.name" />

        <InboundDomainModal v-if="isAddOpen" :is-submitting="isRequesting" :error="requestError" :challenge="issued" @close="closeAdd" @submit="requestDomain" />
        <InboundLimitsModal
            v-if="isLimitsOpen && limits"
            :current="limits"
            :is-submitting="isSaving"
            :error="saveError"
            :blocker="isSignedIn ? null : 'Sign in again to change the limits.'"
            @close="closeLimits"
            @submit="saveLimits"
        />
    </div>
</template>

<script lang="ts" setup>
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { formatDateTime } from '@/format';
import {
    MX_DESCRIPTIONS,
    parseInboundChallenge,
    parseInboundDomain,
    parseInboundDomainList,
    parseInboundLimits,
    STATE_LABELS,
    STATE_TONES,
    type InboundChallenge
} from '@/inbound';
import { DefaultApi, type InboundDomain, type InboundLimits } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import InboundDomainModal from '@/shared/components/inbound-domain-modal.vue';
import InboundLimitsModal from '@/shared/components/inbound-limits-modal.vue';
import InboundMessageList from '@/shared/components/inbound-message-list.vue';
import Loader from '@/shared/components/loader.vue';
import { formatCount } from '@/submission-rate';

type DomainAction = 'verify' | 'records' | 'mx' | 'disable' | 'enable' | 'delete';

const { app, appId } = useAppContext();

const domains = shallowRef<InboundDomain[] | null>(null);
const mxHost = ref<string | null>(null);
const isLoading = ref(true);
const loadError = ref<ApiErrorDescription | null>(null);
const actionError = ref<ApiErrorDescription | null>(null);
const busy = ref<{ domain: string; action: DomainAction } | null>(null);

const isAddOpen = ref(false);
const isRequesting = ref(false);
const requestError = ref<ApiErrorDescription | null>(null);
// The TXT value cannot be read back from the API; it lives only in this component's memory.
const issued = shallowRef<InboundChallenge | null>(null);

// Whether the app has an enabled inbound mail webhook; null until known, and when the list can't be read.
const hasInboundWebhook = ref<boolean | null>(null);

const limits = shallowRef<InboundLimits | null>(null);
const limitsLoading = ref(false);
const limitsError = ref<ApiErrorDescription | null>(null);
const isLimitsOpen = ref(false);
const isSaving = ref(false);
const saveError = ref<ApiErrorDescription | null>(null);

// The Webhooks page opens its add dialog with the inbound format chosen (the format stays editable there). The query carries no data.
const addWebhookTarget = computed(() => ({ name: 'app-webhooks', params: { appId: appId.value }, query: { add: 'thinpost_inbound' } }));

const isSignedIn = computed(() => authState.status === 'signed-in');
const canRequest = computed(() => app.value.active && isSignedIn.value && !isRequesting.value && !busy.value && !loadError.value && !isLoading.value);
const canAct = computed(() => isSignedIn.value && !busy.value && !isRequesting.value);

let loadSeq = 0;
let limitsSeq = 0;
let webhooksSeq = 0;
let isUnmounted = false;
onBeforeUnmount(() => {
    isUnmounted = true;
    issued.value = null;
});

watch(
    appId,
    () => {
        void load();
        void loadLimits();
        void loadWebhooks();
    },
    { immediate: true }
);

async function load() {
    const id = appId.value;
    const seq = ++loadSeq;
    isLoading.value = true;
    loadError.value = null;
    actionError.value = null;
    domains.value = null;
    issued.value = null;
    try {
        const { data } = await DefaultApi.listInboundDomains({ path: { appId: id } });
        if (seq !== loadSeq) return;
        const parsed = parseInboundDomainList(data);
        domains.value = parsed.domains;
        mxHost.value = parsed.mxHost;
    } catch (err) {
        if (seq !== loadSeq) return;
        loadError.value = describeApiError(err, {
            notFound: 'The application was not found.',
            unavailable: 'Receiving domains are unavailable right now. Try again in a moment.'
        });
    } finally {
        if (seq === loadSeq) isLoading.value = false;
    }
}

/** Refreshes the list after a change, keeping the page usable if the refresh itself fails. */
async function reload() {
    const seq = loadSeq;
    try {
        const { data } = await DefaultApi.listInboundDomains({ path: { appId: appId.value } });
        if (seq !== loadSeq || isUnmounted) return;
        const parsed = parseInboundDomainList(data);
        domains.value = parsed.domains;
        mxHost.value = parsed.mxHost;
    } catch (err) {
        if (seq !== loadSeq || isUnmounted) return;
        domains.value = null;
        loadError.value = describeApiError(err, { notFound: 'The application was not found.' });
    }
}

async function loadWebhooks() {
    const seq = ++webhooksSeq;
    hasInboundWebhook.value = null;
    try {
        const { data } = await DefaultApi.listAppWebhooks({ path: { appId: appId.value } });
        if (seq !== webhooksSeq || isUnmounted) return;
        const list: unknown = data?.webhooks;
        if (!Array.isArray(list)) return;
        hasInboundWebhook.value = list.some(hook => hook?.format === 'thinpost_inbound' && hook.active === true);
    } catch {
        // The warning is only a hint; without the list it is left out rather than guessed.
    }
}

async function loadLimits() {
    const seq = ++limitsSeq;
    limitsLoading.value = true;
    limitsError.value = null;
    try {
        const { data } = await DefaultApi.getInboundLimits({ path: { appId: appId.value } });
        if (seq !== limitsSeq) return;
        limits.value = parseInboundLimits(data);
    } catch (err) {
        if (seq !== limitsSeq) return;
        limits.value = null;
        limitsError.value = describeApiError(err, {
            notFound: 'The application was not found.',
            unavailable: 'Limits are unavailable right now. Try again in a moment.'
        });
    } finally {
        if (seq === limitsSeq) limitsLoading.value = false;
    }
}

function isBusy(domain: string, action: DomainAction) {
    return busy.value?.domain === domain && busy.value.action === action;
}

function openAdd() {
    requestError.value = null;
    actionError.value = null;
    issued.value = null;
    isAddOpen.value = true;
}

function closeAdd() {
    if (isRequesting.value) return;
    isAddOpen.value = false;
    issued.value = null;
}

function describeRequestError(err: unknown): ApiErrorDescription {
    const description = describeApiError(err, {
        mutation: true,
        notFound: 'The application was not found or is paused.',
        conflict: 'This domain is already set up for this application.'
    });
    if (description.kind === 'invalid') {
        return { ...description, message: "This domain can't be used. It may not be a valid domain name, or Thinpost may reserve it for its own mail." };
    }
    return description;
}

async function issueRecords(domain: string): Promise<InboundChallenge> {
    const { data } = await DefaultApi.requestInboundDomain({ path: { appId: appId.value }, body: { domain } });
    return parseInboundChallenge(data, domain);
}

async function requestDomain(domain: string) {
    if (isRequesting.value) return;
    const seq = loadSeq;
    isRequesting.value = true;
    requestError.value = null;
    try {
        const challenge = await issueRecords(domain);
        if (isUnmounted || seq !== loadSeq) return;
        issued.value = challenge;
        void reload();
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        requestError.value = describeRequestError(err);
    } finally {
        isRequesting.value = false;
    }
}

async function newRecords(domain: string) {
    if (busy.value) return;
    const seq = loadSeq;
    const ok = await showConfirm('Get new DNS records', `Get a new TXT record for ${domain}? The current TXT value will stop working, so publish the new one instead.`);
    if (!ok || isUnmounted || seq !== loadSeq || !canAct.value) return;
    busy.value = { domain, action: 'records' };
    actionError.value = null;
    try {
        const challenge = await issueRecords(domain);
        if (isUnmounted || seq !== loadSeq) return;
        issued.value = challenge;
        requestError.value = null;
        isAddOpen.value = true;
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        actionError.value = describeRequestError(err);
    } finally {
        busy.value = null;
    }
    if (seq === loadSeq && !isUnmounted) await reload();
}

/** Runs one domain action, then refreshes the list whatever the outcome. */
async function act(domain: string, action: DomainAction, run: () => Promise<unknown>, success: string | null, describe: (err: unknown) => ApiErrorDescription) {
    if (busy.value) return;
    const seq = loadSeq;
    busy.value = { domain, action };
    actionError.value = null;
    try {
        await run();
        if (isUnmounted || seq !== loadSeq) return;
        if (success) showToast({ message: success, durationSecs: 3 });
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        actionError.value = describe(err);
    } finally {
        busy.value = null;
    }
    if (seq === loadSeq && !isUnmounted) await reload();
}

function verify(domain: string) {
    return act(
        domain,
        'verify',
        async () => {
            const { data } = await DefaultApi.verifyInboundDomain({ path: { appId: appId.value, domain } });
            if (parseInboundDomain(data).state === 'pending') throw new InvalidResponseError();
        },
        `${domain} verified`,
        err => {
            // The API maps a TXT record that is not visible yet, and any lookup failure, to "unavailable".
            if (apiErrorStatus(err) === 503) {
                return {
                    kind: 'unavailable',
                    status: 503,
                    message: `Couldn't finish verifying. The TXT record may not be visible yet, or Thinpost is temporarily unavailable. Try again in a moment.`
                };
            }
            return describeApiError(err, {
                mutation: true,
                notFound: `${domain} was not found for this application.`,
                conflict: `The TXT record at _thinpost.${domain} doesn't match, or another application already owns this domain. Check the record, or get new DNS records.`
            });
        }
    );
}

function checkMx(domain: string) {
    return act(
        domain,
        'mx',
        async () => {
            const { data } = await DefaultApi.checkInboundDomainMx({ path: { appId: appId.value, domain } });
            parseInboundDomain(data);
        },
        null,
        err =>
            describeApiError(err, {
                notFound: `${domain} was not found for this application.`,
                unavailable: "The MX check isn't available right now. Ask your Thinpost administrator whether the receiving mail server is set up."
            })
    );
}

function setEnabled(domain: string, enabled: boolean) {
    return act(
        domain,
        enabled ? 'enable' : 'disable',
        async () => {
            const call = enabled ? DefaultApi.enableInboundDomain : DefaultApi.disableInboundDomain;
            const { data } = await call({ path: { appId: appId.value, domain } });
            parseInboundDomain(data);
        },
        enabled ? `${domain} enabled` : `${domain} disabled`,
        err =>
            describeApiError(err, {
                mutation: true,
                notFound: `${domain} was not found for this application.`,
                conflict: `${domain} isn't verified yet. Verify it first.`
            })
    );
}

async function remove(domain: string) {
    if (busy.value) return;
    const seq = loadSeq;
    const ok = await showConfirm('Delete receiving domain', `Delete ${domain}? Mail for it will no longer reach this application, and another application can claim the domain.`);
    if (!ok || isUnmounted || seq !== loadSeq || !canAct.value) return;
    await act(
        domain,
        'delete',
        async () => {
            await DefaultApi.deleteInboundDomain({ path: { appId: appId.value, domain } });
        },
        `${domain} deleted`,
        err => describeApiError(err, { mutation: true, notFound: `${domain} was already removed.` })
    );
}

function openLimits() {
    if (!limits.value || !isSignedIn.value || isSaving.value) return;
    saveError.value = null;
    isLimitsOpen.value = true;
}

function closeLimits() {
    if (isSaving.value) return;
    isLimitsOpen.value = false;
}

async function saveLimits(requested: InboundLimits) {
    if (isSaving.value || !isSignedIn.value) return;
    const target = app.value;
    isSaving.value = true;
    saveError.value = null;
    // A reload that started earlier must not overwrite the saved values.
    limitsSeq++;
    limitsLoading.value = false;
    try {
        const { data } = await DefaultApi.setInboundLimits({ path: { appId: target.id }, body: requested });
        const saved = parseInboundLimits(data);
        if (isUnmounted) {
            showToast({ message: `Inbound limits for “${target.name}” were saved.`, durationSecs: 5 });
            return;
        }
        limits.value = saved;
        limitsError.value = null;
        isLimitsOpen.value = false;
        showToast({ message: `Inbound limits for “${target.name}” saved.`, durationSecs: 3 });
    } catch (err) {
        if (isUnmounted) return;
        saveError.value = describeApiError(err, {
            mutation: true,
            notFound: 'The application was not found. Nothing was changed.',
            unavailable: "Limits can't be changed right now. Try again in a moment."
        });
    } finally {
        isSaving.value = false;
    }
}
</script>

<style lang="scss" scoped>
.inbound-body {
    display: flex;
    flex-direction: column;
    gap: 12px;
}

/* Hide the body when every child rendered nothing (only v-if comments remain). */
.inbound-body.inbound-body:not(:has(*)) {
    display: none;
}

.name {
    font-weight: 600;
    overflow-wrap: anywhere;
}

.checked {
    margin-left: 6px;
    font-size: 12px;
}

.header-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
}

.actions {
    text-align: right;
    white-space: nowrap;

    .btn + .btn {
        margin-left: 8px;
    }
}

.mx-note {
    display: block;
    margin-top: 2px;
    font-size: 12px;
    font-weight: 400;
    white-space: nowrap;
}
</style>
