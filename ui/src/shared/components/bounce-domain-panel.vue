<template>
    <article class="card bounce" :aria-labelledby="`${id}-title`" :data-testid="`bounce-${domain.name}`">
        <div class="bounce-header">
            <div class="heading-with-help">
                <h3 :id="`${id}-title`">Bounce domain for {{ domain.name }}</h3>
                <HelpButton topic="bounce domains">
                    <p>
                        Bounces go back to a hidden return address, normally one of Thinpost's names. A bounce domain is a name of yours, such as <code>{{ suggestBounceName(domain.name) }}</code
                        >, used instead, so SPF passes for your domain too.
                    </p>
                    <p>Add one CNAME to the target shown, <strong>DNS only, not proxied</strong>. Thinpost rechecks every 10 minutes at first, then every 6 hours.</p>
                    <p>Mail uses it only while the record is right. Otherwise it falls back to Thinpost's bounce address, and nothing is lost.</p>
                    <p>Also covers verified subdomains that don't have their own.</p>
                </HelpButton>
                <span v-if="view" class="tag" :class="view.tag.tone" data-testid="bounce-status">{{ view.tag.label }}</span>
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

        <div v-if="bounce && view" class="records">
            <CnameRecord :record="view.record" :note="view.note" :checked-at="bounce.checkedAt" />
        </div>
        <template v-else>
            <p v-if="inherited" class="hint" data-testid="bounce-empty">Uses {{ inherited.name }} from {{ inherited.from }}. Add your own to override.</p>
            <p v-else class="hint" data-testid="bounce-empty">Uses Thinpost's bounce address. Add your own to align SPF.</p>
            <p v-if="!appActive" class="hint">This app is paused, so this can't be changed.</p>
        </template>

        <BounceDomainModal v-if="isModalOpen" :sender="domain.name" :current="bounce?.name ?? null" :is-submitting="isSaving" :error="modalError" @close="closeModal" @submit="save" />
    </article>
</template>

<script setup lang="ts">
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, useId } from 'vue';

import { describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { bounceView, parseBounceDomain, parseCnameTarget, suggestBounceName } from '@/bounce-domains';
import { DefaultApi, type Domain } from '@/openapi-client-generated';

import ApiErrorNotice from './api-error-notice.vue';
import BounceDomainModal from './bounce-domain-modal.vue';
import CnameRecord from './cname-record.vue';
import HelpButton from './help-button.vue';

const props = defineProps<{
    appId: string;
    appActive: boolean;
    /** False while another action on the screen is running or the person has signed out. */
    canAct: boolean;
    /** A verified sending domain. */
    domain: Domain;
    /** The parent sending domain whose bounce domain this one's mail uses, when it has none of its own. */
    inherited?: { from: string; name: string } | null;
}>();

const emit = defineEmits<{
    /** The bounce domain changed on the server, so the list should be read again. */
    changed: [];
}>();

const id = useId();
const bounce = computed(() => parseBounceDomain(props.domain.bounce_domain));
const target = computed(() => parseCnameTarget(props.domain));
const view = computed(() => (bounce.value ? bounceView(bounce.value, target.value) : null));

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

.records {
    display: flex;
    flex-direction: column;
}
</style>
