<template>
    <article class="card bounce" :aria-labelledby="`${id}-title`" :data-testid="`bounce-${domain.name}`">
        <div class="bounce-header">
            <div class="heading-with-help">
                <h3 :id="`${id}-title`">Bounce domain for {{ domain.name }}</h3>
                <HelpButton topic="bounce domains">
                    <p>
                        Every email has a hidden return address that bounces go back to. By default it is one of Thinpost's own names, which is fine for delivery but isn't your domain. A bounce
                        domain is a name of yours, such as <code>{{ suggestBounceName(domain.name) }}</code
                        >, that Thinpost uses instead, so SPF passes for your domain too.
                    </p>
                    <ul>
                        <li>You add one <strong>CNAME</strong> record for it, pointing at the name shown here. It must be <strong>DNS only, not proxied</strong> (turn off an orange-cloud proxy).</li>
                        <li>Mail uses it only while Thinpost can see the record. If the record goes missing or changes, mail goes back to Thinpost's standard bounce address on its own. Nothing is lost.</li>
                        <li>It lines up with DMARC's usual (relaxed) SPF check. If your DMARC record asks for strict SPF (<code>aspf=s</code>), DKIM still carries it.</li>
                        <li>If this app starts sending from a different IP, the CNAME needs to point at that IP's name. This page shows the new name.</li>
                        <li>It also covers mail from verified subdomains of {{ domain.name }}, using the most specific one that is in use: a subdomain whose own bounce domain isn't in use yet falls back to this one.</li>
                    </ul>
                </HelpButton>
            </div>
            <div v-if="bounce" class="buttons">
                <button type="button" class="btn sm" :disabled="!canAct || busy !== null" @click="check">
                    {{ isBusy('check') || bounce.checking ? 'Checking…' : 'Check now' }}
                    <span class="sr-only">the bounce domain for {{ domain.name }}</span>
                </button>
                <button type="button" class="btn sm" :disabled="!canAct || busy !== null || !appActive" @click="openModal">
                    Change
                    <span class="sr-only">the bounce domain for {{ domain.name }}</span>
                </button>
                <button type="button" class="btn sm" :disabled="!canAct || busy !== null" @click="remove">
                    {{ isBusy('remove') ? 'Removing…' : 'Remove' }}
                    <span class="sr-only">the bounce domain for {{ domain.name }}</span>
                </button>
            </div>
            <button v-else type="button" class="btn sm primary" :disabled="!canAct || !appActive" @click="openModal">
                Add bounce domain
                <span class="sr-only">for {{ domain.name }}</span>
            </button>
        </div>

        <ApiErrorNotice v-if="error" :error="error" />

        <template v-if="bounce && status">
            <p class="line">
                <strong class="name">{{ bounce.name }}</strong>
                <span class="tag" :class="status.tone" data-testid="bounce-status">{{ status.label }}</span>
            </p>
            <p class="hint" data-testid="bounce-message">{{ status.message }}</p>
            <div v-if="target.target && bounce.state !== 'active'" class="record">
                <CopyField label="CNAME record name" :value="bounce.name" />
                <CopyField label="CNAME record value" :value="target.target" />
                <p class="hint">Add it as a CNAME record, DNS only, not proxied. DNS changes can take a while to spread, so Thinpost checks again every 10 minutes for the first 48 hours, then every 6 hours. Choose Check now to look straight away.</p>
            </div>
            <p v-else-if="target.target" class="hint">
                The record is a CNAME from <code>{{ bounce.name }}</code> to <code>{{ target.target }}</code
                >, DNS only.
            </p>
            <p v-if="bounce.checkedAt" class="hint">
                Last checked <time :datetime="bounce.checkedAt">{{ formatDateTime(bounce.checkedAt) }}</time>.
            </p>
        </template>
        <template v-else>
            <p class="hint">
                Mail from {{ domain.name }} uses Thinpost's standard bounce address. To use your own, such as <code>{{ suggestBounceName(domain.name) }}</code
                >, add a bounce domain and point one CNAME record at Thinpost.
            </p>
            <p v-if="!appActive" class="hint">This app is paused, so its bounce domain can't be changed.</p>
        </template>

        <BounceDomainModal v-if="isModalOpen" :sender="domain.name" :current="bounce?.name ?? null" :is-submitting="isSaving" :error="modalError" @close="closeModal" @submit="save" />
    </article>
</template>

<script setup lang="ts">
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, useId } from 'vue';

import { describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { bounceStatus, parseBounceDomain, parseCnameTarget, suggestBounceName } from '@/bounce-domains';
import { formatDateTime } from '@/format';
import { DefaultApi, type Domain } from '@/openapi-client-generated';

import ApiErrorNotice from './api-error-notice.vue';
import BounceDomainModal from './bounce-domain-modal.vue';
import CopyField from './copy-field.vue';
import HelpButton from './help-button.vue';

const props = defineProps<{
    appId: string;
    appActive: boolean;
    /** False while another action on the screen is running or the person has signed out. */
    canAct: boolean;
    /** A verified sending domain. */
    domain: Domain;
}>();

const emit = defineEmits<{
    /** The bounce domain changed on the server, so the list should be read again. */
    changed: [];
}>();

const id = useId();
const bounce = computed(() => parseBounceDomain(props.domain.bounce_domain));
const target = computed(() => parseCnameTarget(props.domain));
const status = computed(() => (bounce.value ? bounceStatus(bounce.value, props.domain.name, target.value) : null));

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
        const { data } = await DefaultApi.setAppBounceDomain({ path: { appId: props.appId, domain: props.domain.name }, body: { name } });
        if (!parseBounceDomain(data)) throw new InvalidResponseError();
        if (isUnmounted) return;
        isModalOpen.value = false;
        showToast({ message: `Bounce domain saved for ${props.domain.name}`, durationSecs: 3 });
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
        conflict: "That name can't be used. It may already receive mail through Thinpost, belong to another sending domain of this app, or the app may be paused."
    });
    if (described.status === 400) {
        return {
            ...described,
            message: `Use a subdomain of ${props.domain.name}, such as ${suggestBounceName(props.domain.name)}. It can't be one of Thinpost's own names, and ${props.domain.name} must be verified.`
        };
    }
    return described;
}

async function check() {
    if (busy.value) return;
    busy.value = 'check';
    error.value = null;
    try {
        const { data } = await DefaultApi.checkAppBounceDomain({ path: { appId: props.appId, domain: props.domain.name } });
        if (!parseBounceDomain(data)) throw new InvalidResponseError();
    } catch (err) {
        if (isUnmounted) return;
        error.value = describeApiError(err, {
            notFound: 'This sending domain has no bounce domain any more. Reload to see where it stands.',
            conflict: 'Another check was already running. Wait a moment, then check again.',
            unavailable: "Thinpost couldn't look up DNS just now. Try again in a moment."
        });
    } finally {
        busy.value = null;
    }
    if (!isUnmounted) emit('changed');
}

async function remove() {
    if (busy.value || !bounce.value) return;
    const ok = await showConfirm(
        'Remove the bounce domain?',
        `Mail from ${props.domain.name} goes back to Thinpost's standard bounce address from the next message. Bounces already on their way to ${bounce.value.name} are still received. You can remove its CNAME record from DNS.`
    );
    if (!ok || isUnmounted || !props.canAct) return;
    busy.value = 'remove';
    error.value = null;
    try {
        await DefaultApi.deleteAppBounceDomain({ path: { appId: props.appId, domain: props.domain.name } });
        if (isUnmounted) return;
        showToast({ message: `Bounce domain removed for ${props.domain.name}`, durationSecs: 3 });
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
.bounce {
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding: 16px 18px;
    box-shadow: none;
}

.bounce-header {
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
