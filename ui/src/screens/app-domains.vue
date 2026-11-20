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
                            <strong>Verified means you own the domain, not that mail is ready to send.</strong> A domain is verified once Thinpost
                            can see the TXT record you add at <code>_thinpost.&lt;domain&gt;</code>.
                        </p>
                        <p>
                            Verifying ownership doesn't set up or check DKIM, SPF, DMARC or reverse DNS, so it doesn't mean mail from the domain is
                            ready to send or will reach inboxes. Sending also needs a DKIM key, which your Thinpost administrator sets up. This
                            console can't show those settings. Domains can't be removed here.
                        </p>
                    </HelpButton>
                </div>
                <button type="button" class="btn primary" :disabled="!canRequest" :aria-describedby="app.active ? undefined : 'request-hint'" @click="openRequest">
                    <i class="fa-solid fa-plus" aria-hidden="true" />
                    Add domain
                </button>
            </div>

            <div class="card-body domains-body">
                <p v-if="!app.active" id="request-hint" class="hint">This app is paused, so TXT records can't be requested.</p>
                <p v-if="testSend.blocker.value && testSend.blocker.value !== pausedHint" class="hint">{{ testSend.blocker.value }}</p>
                <ApiErrorNotice v-if="testSend.outcomeError.value" :error="testSend.outcomeError.value" />

                <ApiErrorNotice v-if="actionError" :error="actionError" />

                <div v-if="challenges.length" class="challenges" aria-label="TXT records requested on this page">
                    <article v-for="challenge in challenges" :key="challenge.domain" class="card challenge" :aria-labelledby="`challenge-${challenge.domain}`">
                        <div class="challenge-header">
                            <div class="heading-with-help">
                                <h3 :id="`challenge-${challenge.domain}`">
                                    TXT record for {{ challenge.domain }}
                                </h3>
                                <HelpButton topic="TXT records">
                                    <p>
                                        Add this TXT record at your DNS provider, then choose Verify. The value is shown only here, and Thinpost
                                        doesn't keep a copy you can read. If you lose it, request a new record; that makes this value stop working.
                                    </p>
                                </HelpButton>
                            </div>
                            <button type="button" class="btn sm" @click="dismissChallenge(challenge.domain)">
                                Dismiss
                                <span class="sr-only">TXT record for {{ challenge.domain }}</span>
                            </button>
                        </div>
                        <p class="hint">Shown once. Requesting a new record makes this one stop working.</p>
                        <div class="challenge-fields">
                            <CopyField label="Record name" :value="challenge.dnsName" />
                            <CopyField label="Record value" :value="challenge.dnsValue" />
                        </div>
                    </article>
                </div>

                <Loader v-if="isReloading && !domains" label="Loading sending domains" />
                <ApiErrorNotice v-else-if="domainsError" :error="domainsError" :retry="reloadDomains" />

                <div v-else-if="domains && !domains.length" class="empty">
                    <i class="fa-solid fa-globe" aria-hidden="true" />
                    <h2>No sending domains yet</h2>
                </div>
            </div>

            <div v-if="domains && domains.length && !domainsError" class="table-wrap" :aria-busy="isReloading">
                <table class="data">
                    <caption class="sr-only">Sending domains for {{ app.name }}</caption>
                    <thead>
                        <tr>
                            <th scope="col">Domain</th>
                            <th scope="col">Status</th>
                            <th scope="col">DKIM / SPF</th>
                            <th scope="col">Added</th>
                            <th scope="col">Verified</th>
                            <th scope="col"><span class="sr-only">Actions</span></th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr v-for="domain in domains" :key="domain.name">
                            <th scope="row" class="name">{{ domain.name }}</th>
                            <td>
                                <span class="tag" :class="domain.active ? 'success' : 'warning'">{{ domain.active ? 'Verified' : 'Not verified' }}</span>
                            </td>
                            <td>
                                <span class="tag" title="Thinpost doesn't report DKIM or SPF status">Not shown</span>
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
                                    v-if="domain.active"
                                    type="button"
                                    class="btn sm"
                                    :disabled="!!testSend.blocker.value || !canAct"
                                    @click="testSend.open(domain.name)"
                                >
                                    Send test
                                    <span class="sr-only">from {{ domain.name }}</span>
                                </button>
                                <template v-if="!domain.active">
                                    <button type="button" class="btn sm" :disabled="!canAct" @click="verify(domain.name)">
                                        {{ isBusy(domain.name, 'verify') ? 'Verifying…' : 'Verify' }}
                                        <span class="sr-only">{{ domain.name }}</span>
                                    </button>
                                    <button type="button" class="btn sm" :disabled="!canAct || !app.active" @click="reissue(domain.name)">
                                        {{ isBusy(domain.name, 'reissue') ? 'Requesting…' : 'New TXT record' }}
                                        <span class="sr-only">for {{ domain.name }}</span>
                                    </button>
                                </template>
                            </td>
                        </tr>
                    </tbody>
                </table>
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

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { formatDateTime } from '@/format';
import { DefaultApi, type Domain } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import CopyField from '@/shared/components/copy-field.vue';
import DomainModal from '@/shared/components/domain-modal.vue';
import Loader from '@/shared/components/loader.vue';
import TestMessageModal from '@/shared/components/test-message-modal.vue';
import { useTestSend } from '@/test-send';

interface IssuedChallenge {
    domain: string;
    dnsName: string;
    dnsValue: string;
}

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
const busy = ref<{ domain: string; action: 'verify' | 'reissue' } | null>(null);

// TXT values cannot be read back from the API; they are kept only in this component's memory.
const challenges = shallowRef<IssuedChallenge[]>([]);

const isSignedIn = computed(() => authState.status === 'signed-in');
const canRequest = computed(() => app.value.active && isSignedIn.value && !isRequesting.value && !busy.value);
const canAct = computed(() => isSignedIn.value && !busy.value && !isRequesting.value);

let loadSeq = 0;
let reloadSeq = 0;
let isUnmounted = false;
onBeforeUnmount(() => {
    isUnmounted = true;
    challenges.value = [];
});

watch(appId, load, { immediate: true });

async function load() {
    const id = appId.value;
    const seq = ++loadSeq;
    isLoading.value = true;
    loadError.value = null;
    domainsError.value = null;
    actionError.value = null;
    domains.value = null;
    challenges.value = [];

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

function isBusy(domain: string, action: 'verify' | 'reissue') {
    return busy.value?.domain === domain && busy.value.action === action;
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

const CHALLENGE_ERRORS = {
    mutation: true,
    notFound: 'The app was not found, or it is paused.',
    conflict: 'This domain is already verified for this app, so it doesn\'t need a new TXT record.'
};

/** Requests or reissues a TXT challenge; the API uses the same call for both. */
async function issueChallenge(appId: string, domain: string): Promise<IssuedChallenge> {
    const { data } = await DefaultApi.requestAppDomain({ path: { appId }, body: { domain } });
    if (!data || typeof data.dns_name !== 'string' || typeof data.dns_value !== 'string' || !data.dns_name || !data.dns_value) {
        throw new InvalidResponseError();
    }
    return { domain: typeof data.domain?.name === 'string' ? data.domain.name : domain, dnsName: data.dns_name, dnsValue: data.dns_value };
}

function showChallenge(challenge: IssuedChallenge) {
    challenges.value = [challenge, ...challenges.value.filter(c => c.domain !== challenge.domain)];
}

function dismissChallenge(domain: string) {
    challenges.value = challenges.value.filter(c => c.domain !== domain);
}

async function requestDomain(domain: string) {
    const appId = app.value?.id;
    if (!appId || isRequesting.value) return;
    const seq = loadSeq;
    // Requesting an existing unverified domain replaces its challenge, exactly like reissuing.
    if (domains.value?.some(d => d.name === domain && !d.active)) {
        isRequesting.value = true;
        const ok = await showConfirm(
            'Request a new TXT record?',
            `${domain} is already added but not verified. Requesting it again gives you a new TXT value, and the current one will stop working.`
        );
        isRequesting.value = false;
        if (!ok || isUnmounted || seq !== loadSeq) return;
    }
    isRequesting.value = true;
    requestError.value = null;
    try {
        const challenge = await issueChallenge(appId, domain);
        if (isUnmounted || seq !== loadSeq) return;
        isRequestOpen.value = false;
        showChallenge(challenge);
        showToast({ message: `TXT record ready for ${challenge.domain}`, durationSecs: 3 });
        void reloadDomains();
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        requestError.value = describeApiError(err, CHALLENGE_ERRORS);
    } finally {
        isRequesting.value = false;
    }
}

async function reissue(domain: string) {
    const appId = app.value?.id;
    if (!appId || busy.value) return;
    const seq = loadSeq;
    const ok = await showConfirm(
        'Request a new TXT record?',
        `Request a new TXT record for ${domain}? The current value will stop working; add the new one instead.`
    );
    if (!ok || isUnmounted || seq !== loadSeq || !canAct.value) return;
    busy.value = { domain, action: 'reissue' };
    actionError.value = null;
    try {
        const challenge = await issueChallenge(appId, domain);
        if (isUnmounted || seq !== loadSeq) return;
        showChallenge(challenge);
        showToast({ message: `New TXT record ready for ${challenge.domain}`, durationSecs: 3 });
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        actionError.value = describeApiError(err, CHALLENGE_ERRORS);
    } finally {
        busy.value = null;
    }
    if (seq === loadSeq && !isUnmounted) await reloadDomains();
}

function describeVerifyError(err: unknown, domain: string): ApiErrorDescription {
    const record = `_thinpost.${domain}`;
    // The API reports any TXT lookup problem, including a record that does not exist yet, as unavailable.
    if (apiErrorStatus(err) === 503) {
        return {
            kind: 'unavailable',
            status: 503,
            message: `Thinpost can't see a TXT record at ${record} yet, or the DNS lookup failed. DNS changes can take a while to spread, so try again later. The list shows the current status.`
        };
    }
    return describeApiError(err, {
        mutation: true,
        notFound: `${domain} wasn't found for this app.`,
        conflict: `TXT records exist at ${record}, but none matches the current value, or no value has been requested for this domain. Check the value you added, or request a new TXT record.`
    });
}

async function verify(domain: string) {
    const appId = app.value?.id;
    if (!appId || busy.value) return;
    const seq = loadSeq;
    busy.value = { domain, action: 'verify' };
    actionError.value = null;
    try {
        const { data } = await DefaultApi.verifyAppDomain({ path: { appId, domain } });
        if (isUnmounted || seq !== loadSeq) return;
        if (data?.active !== true) throw new InvalidResponseError();
        dismissChallenge(domain);
        showToast({ message: `${domain} verified`, durationSecs: 3 });
    } catch (err) {
        if (isUnmounted || seq !== loadSeq) return;
        actionError.value = describeVerifyError(err, domain);
    } finally {
        busy.value = null;
    }
    if (seq === loadSeq && !isUnmounted) await reloadDomains();
}
</script>

<style lang="scss" scoped>
.domains-body {
    display: flex;
    flex-direction: column;
    gap: 12px;
}

/* Hide the body when every child rendered nothing (only v-if comments remain). */
.domains-body.domains-body:not(:has(*)) {
    display: none;
}

.explainer {
    display: flex;
    flex-direction: column;
    gap: 8px;
}

.challenges {
    display: flex;
    flex-direction: column;
    gap: 12px;
}

.challenge {
    display: flex;
    flex-direction: column;
    gap: 12px;
    padding: 16px 18px;
    box-shadow: none;
}

.challenge-header {
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

.challenge-fields {
    display: grid;
    gap: 12px;
}

.name {
    font-weight: 600;
    overflow-wrap: anywhere;
}

.actions {
    text-align: right;
    white-space: nowrap;

    .btn + .btn {
        margin-left: 8px;
    }
}
</style>
