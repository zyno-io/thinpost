<template>
    <article class="card tracking" :aria-labelledby="`${id}-title`" :data-testid="`tracking-${domain.name}`">
        <div class="tracking-header">
            <div class="heading-with-help">
                <h3 :id="`${id}-title`">Tracking domain for {{ domain.name }}</h3>
                <HelpButton topic="tracking domains">
                    <p>
                        Tracked links and the open-tracking image point at a web address, normally Thinpost's. A tracking domain is a name of yours, such as <code>{{ suggestTrackingName(domain.name) }}</code
                        >, used instead, so recipients see your domain.
                    </p>
                    <p>Add a CNAME to the target shown. A proxy such as Cloudflare is fine. Your Thinpost administrator finishes setup with a security certificate.</p>
                    <p>New mail uses it only once Thinpost can open it over HTTPS. Otherwise it uses Thinpost's address, and nothing is lost. Links already sent need the record and certificate to stay in place, and one-click unsubscribe links always use Thinpost's address.</p>
                    <p>Also covers verified subdomains that don't have their own.</p>
                </HelpButton>
                <span v-if="view" class="tag" :class="view.tag.tone" data-testid="tracking-status">{{ view.tag.label }}</span>
            </div>
            <div v-if="tracking" class="buttons">
                <button type="button" class="btn sm" :disabled="!canAct || busy !== null" @click="check">
                    {{ isBusy('check') || tracking.checking ? 'Checking…' : 'Check now' }}
                    <span class="sr-only">the tracking domain for {{ domain.name }}</span>
                </button>
                <button v-if="support.available" type="button" class="btn sm" :disabled="!canAct || busy !== null || !appActive" @click="openModal">
                    Change
                    <span class="sr-only">the tracking domain for {{ domain.name }}</span>
                </button>
                <button type="button" class="btn sm" :disabled="!canAct || busy !== null" @click="remove">
                    {{ isBusy('remove') ? 'Removing…' : 'Remove' }}
                    <span class="sr-only">the tracking domain for {{ domain.name }}</span>
                </button>
            </div>
            <button v-else-if="support.available" type="button" class="btn sm primary" :disabled="!canAct || !appActive" @click="openModal">
                Add tracking domain
                <span class="sr-only">for {{ domain.name }}</span>
            </button>
        </div>

        <ApiErrorNotice v-if="error" :error="error" />

        <div v-if="tracking && view" class="records">
            <CnameRecord :record="view.record" :note="view.note" :checked-at="tracking.checkedAt" />
            <DnsRow v-if="view.https" label="HTTPS">
                <p class="line">
                    <span class="tag" :class="view.https.tag.tone" data-testid="tracking-https">{{ view.https.tag.label }}</span>
                    <span v-if="view.https.expiresAt" class="muted"
                        >Certificate expires <time :datetime="view.https.expiresAt">{{ formatDateTime(view.https.expiresAt) }}</time></span
                    >
                </p>
                <p v-if="view.https.line" class="muted" data-testid="tracking-https-note">{{ view.https.line }}</p>
            </DnsRow>
        </div>
        <template v-else>
            <p v-if="inherited" class="hint" data-testid="tracking-empty">Uses {{ inherited.name }} from {{ inherited.from }}. Add your own to override.</p>
            <p v-else class="hint" data-testid="tracking-empty">Uses Thinpost's tracking address. Add your own for branded links.</p>
            <p v-if="!appActive" class="hint">This app is paused, so this can't be changed.</p>
        </template>

        <TrackingDomainModal v-if="isModalOpen" :sender="domain.name" :current="tracking?.name ?? null" :is-submitting="isSaving" :error="modalError" @close="closeModal" @submit="save" />
    </article>
</template>

<script setup lang="ts">
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, useId } from 'vue';

import { describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { formatDateTime } from '@/format';
import { DefaultApi, type Domain } from '@/openapi-client-generated';
import { parseTrackingDomain, suggestTrackingName, trackingView, type TrackingSupport } from '@/tracking-domains';

import ApiErrorNotice from './api-error-notice.vue';
import CnameRecord from './cname-record.vue';
import DnsRow from './dns-row.vue';
import HelpButton from './help-button.vue';
import TrackingDomainModal from './tracking-domain-modal.vue';

const props = defineProps<{
    appId: string;
    appActive: boolean;
    /** False while another action on the screen is running or the person has signed out. */
    canAct: boolean;
    /** A verified sending domain. */
    domain: Domain;
    /** What the deployment offers (from the runtime report). */
    support: TrackingSupport;
    /** The parent sending domain whose tracking domain this one's mail uses, when it has none of its own. */
    inherited?: { from: string; name: string } | null;
}>();

const emit = defineEmits<{
    /** The tracking domain changed on the server, so the list should be read again. */
    changed: [];
}>();

const id = useId();
const tracking = computed(() => parseTrackingDomain(props.domain.tracking_domain));
const view = computed(() => (tracking.value ? trackingView(tracking.value, props.support) : null));

const busy = ref<'check' | 'remove' | null>(null);
const isModalOpen = ref(false);
const isSaving = ref(false);
const error = ref<ApiErrorDescription | null>(null);
const modalError = ref<ApiErrorDescription | null>(null);
let isUnmounted = false;
onBeforeUnmount(() => {
    isUnmounted = true;
});

const isBusy = (action: 'check' | 'remove') => busy.value === action;

function openModal() {
    modalError.value = null;
    error.value = null;
    isModalOpen.value = true;
}

function closeModal() {
    if (!isSaving.value) isModalOpen.value = false;
}

async function save(name: string) {
    if (isSaving.value) return;
    isSaving.value = true;
    modalError.value = null;
    try {
        const { data } = await DefaultApi.setAppTrackingDomain({ path: { appId: props.appId, domain: props.domain.name }, body: { name } });
        if (!parseTrackingDomain(data)) throw new InvalidResponseError();
        if (isUnmounted) return;
        isModalOpen.value = false;
        showToast({ message: `Tracking domain saved for ${props.domain.name}`, durationSecs: 3 });
        emit('changed');
    } catch (err) {
        if (isUnmounted) return;
        modalError.value = describeSaveError(err);
    } finally {
        isSaving.value = false;
    }
}

function describeSaveError(err: unknown): ApiErrorDescription {
    const described = describeApiError(err, {
        mutation: true,
        notFound: 'The app or sending domain was not found.',
        conflict: "That name can't be used. Your own tracking address may not be turned on for this Thinpost installation, the name may belong to another sending domain of this app, or the app may be paused."
    });
    if (described.status === 400) {
        return {
            ...described,
            message: `Use a subdomain of ${props.domain.name}, such as ${suggestTrackingName(props.domain.name)}. It can't be one of Thinpost's own names, and ${props.domain.name} must be verified.`
        };
    }
    return described;
}

async function check() {
    if (busy.value) return;
    busy.value = 'check';
    error.value = null;
    try {
        const { data } = await DefaultApi.checkAppTrackingDomain({ path: { appId: props.appId, domain: props.domain.name } });
        if (!parseTrackingDomain(data)) throw new InvalidResponseError();
    } catch (err) {
        if (isUnmounted) return;
        error.value = describeApiError(err, {
            notFound: 'This sending domain has no tracking domain any more. Reload to see where it stands.',
            conflict: 'Another check was already running. Wait a moment, then check again.',
            unavailable: "Thinpost couldn't run the check just now. Try again in a moment."
        });
    } finally {
        busy.value = null;
    }
    if (!isUnmounted) emit('changed');
}

async function remove() {
    if (busy.value || !tracking.value) return;
    const ok = await showConfirm(
        'Remove the tracking domain?',
        `New mail will use the standard tracking address. Links already sent keep working only while the DNS record for ${tracking.value.name} and its certificate stay in place.`
    );
    if (!ok || isUnmounted || !props.canAct) return;
    busy.value = 'remove';
    error.value = null;
    try {
        await DefaultApi.deleteAppTrackingDomain({ path: { appId: props.appId, domain: props.domain.name } });
        if (isUnmounted) return;
        showToast({ message: `Tracking domain removed for ${props.domain.name}`, durationSecs: 3 });
    } catch (err) {
        if (isUnmounted) return;
        error.value = describeApiError(err, { mutation: true, notFound: 'It was already removed. Reload to see where it stands.' });
    } finally {
        busy.value = null;
    }
    if (!isUnmounted) emit('changed');
}
</script>

<style lang="scss" scoped>
.tracking {
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding: 16px 18px;
    box-shadow: none;
}

.tracking-header {
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

.buttons {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
}

.records {
    display: flex;
    flex-direction: column;
}

.line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
    min-height: 19px;
}

.muted {
    color: var(--text-3);
    font-size: 12px;
}
</style>
