<template>
    <article class="card tracking" :aria-labelledby="`${id}-title`" :data-testid="`tracking-${domain.name}`">
        <div class="tracking-header">
            <div class="heading-with-help">
                <h3 :id="`${id}-title`">Tracking domain for {{ domain.name }}</h3>
                <HelpButton topic="tracking domains">
                    <p>
                        When you track opens and clicks, the links and the tiny image in your emails point at a web address. By default that is Thinpost's own. A tracking domain is a name of yours,
                        such as <code>{{ suggestTrackingName(domain.name) }}</code
                        >, that Thinpost uses instead, so recipients see your domain.
                    </p>
                    <ul>
                        <li>You add a DNS record for it (a CNAME pointing at the name shown here), and your Thinpost administrator finishes setting up the address with a security certificate.</li>
                        <li>New mail uses it only once Thinpost can open it over a secure connection. Until then, and whenever that stops working, links use Thinpost's standard address. Nothing is lost.</li>
                        <li>You can put a proxy such as Cloudflare in front of it. Thinpost only needs the secure connection to work.</li>
                        <li>One-click unsubscribe links keep using Thinpost's standard address, so they don't depend on your DNS.</li>
                        <li>Links already sent keep working only while the DNS record and the certificate stay in place. Keep them for as long as old emails matter.</li>
                        <li>It also covers mail from verified subdomains of {{ domain.name }}, using the most specific one that is in use.</li>
                    </ul>
                </HelpButton>
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

        <template v-if="tracking && status">
            <p class="line">
                <strong class="name">{{ tracking.name }}</strong>
                <span class="tag" :class="status.tone" data-testid="tracking-status">{{ status.label }}</span>
            </p>
            <p class="hint" data-testid="tracking-message">{{ status.message }}</p>
            <div v-if="support.available && support.cnameTarget && tracking.state !== 'active'" class="record">
                <CopyField label="CNAME record name" :value="tracking.name" />
                <CopyField label="CNAME record value" :value="support.cnameTarget" />
                <p class="hint">
                    Add it as a CNAME record. DNS changes can take a while to spread, so Thinpost checks again every 10 minutes for the first 48 hours, then every 6 hours. Choose Check now to look
                    straight away.
                </p>
            </div>
            <p v-if="tracking.certificateExpiresAt" class="hint">
                Its security certificate expires <time :datetime="tracking.certificateExpiresAt">{{ formatDateTime(tracking.certificateExpiresAt) }}</time>.
            </p>
            <p v-if="tracking.checkedAt" class="hint">
                Last checked <time :datetime="tracking.checkedAt">{{ formatDateTime(tracking.checkedAt) }}</time>.
            </p>
        </template>
        <template v-else>
            <p class="hint">
                Tracked links and the open-tracking image in mail from {{ domain.name }} use Thinpost's standard tracking address.
                <template v-if="support.available">To use your own, such as <code>{{ suggestTrackingName(domain.name) }}</code>, add a tracking domain.</template>
                <template v-else>Using your own address isn't turned on for this Thinpost installation. Ask your Thinpost administrator.</template>
            </p>
            <p v-if="support.available && !appActive" class="hint">This app is paused, so its tracking domain can't be changed.</p>
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
import { parseTrackingDomain, suggestTrackingName, trackingStatus, type TrackingSupport } from '@/tracking-domains';

import ApiErrorNotice from './api-error-notice.vue';
import CopyField from './copy-field.vue';
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
}>();

const emit = defineEmits<{
    /** The tracking domain changed on the server, so the list should be read again. */
    changed: [];
}>();

const id = useId();
const tracking = computed(() => parseTrackingDomain(props.domain.tracking_domain));
const status = computed(() => (tracking.value ? trackingStatus(tracking.value, props.domain.name, props.support) : null));

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

.line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px;
}

.name {
    overflow-wrap: anywhere;
}

.record {
    display: grid;
    gap: 12px;
}
</style>
