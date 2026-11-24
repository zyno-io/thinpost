<template>
    <VfModal
        class="tp-modal"
        role="alertdialog"
        aria-modal="true"
        :aria-labelledby="`${id}-title`"
        :aria-describedby="`${id}-warning`"
        data-remove-domain
        @close="cancel"
        @form-submit="submit"
    >
        <template #header>
            <span :id="`${id}-title`">Remove domain?</span>
        </template>

        <div class="modal-body">
            <dl class="target">
                <dt>Domain</dt>
                <dd><code>{{ domain.name }}</code></dd>
            </dl>

            <div :id="`${id}-warning`" class="notice warning">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <ul class="notice-body">
                    <template v-if="domain.active">
                        <li><strong>New mail from {{ domain.name }} is refused</strong> right away.</li>
                        <li>Mail that isn't signed yet ends as preparation failed. Mail that is already signed is still delivered.</li>
                    </template>
                    <li v-else>{{ domain.name }} isn't verified, so no mail is sent from it. Its waiting DKIM key is deleted.</li>
                    <li v-if="domain.bounce_domain">
                        Its bounce domain <code>{{ domain.bounce_domain.name }}</code> is removed.
                    </li>
                    <li v-if="trackingName">
                        Its tracking domain <code>{{ trackingName }}</code> is removed. Old links keep working while its DNS and certificate stay.
                    </li>
                    <li v-if="subdomains.length">
                        <template v-for="(sub, i) in subdomains" :key="sub.name">
                            <code>{{ sub.name }}</code>{{ i < subdomains.length - 1 ? ', ' : '' }}
                        </template>
                        {{ subdomains.length === 1 ? 'uses' : 'use' }} {{ domain.name }}'s bounce or tracking domain, so
                        {{ subdomains.length === 1 ? 'it falls' : 'they fall' }} back to a parent domain's, or to Thinpost's standard addresses.
                    </li>
                    <li v-if="domain.active_key">The DKIM key is deleted for good. Adding {{ domain.name }} again needs a new DNS record.</li>
                    <li>Other apps that use {{ domain.name }} aren't affected.</li>
                </ul>
            </div>

            <div v-if="policyBlocked" class="notice danger" role="alert">
                <i class="fa-solid fa-circle-exclamation" aria-hidden="true" />
                <div class="notice-body">
                    <p>
                        The SMTP From policy sends mail as {{ domain.name }}. Change it on the
                        <RouterLink :to="{ name: 'app-smtp', params: { appId } }">SMTP tab</RouterLink> first.
                    </p>
                </div>
            </div>
            <ApiErrorNotice v-else-if="error" :error="error" />
            <p v-if="blocker" class="blocker" role="status">{{ blocker }}</p>

            <template v-if="domain.active">
                <label :for="`${id}-confirm`">Type the domain name to confirm</label>
                <input
                    :id="`${id}-confirm`"
                    v-model="typed"
                    v-autofocus
                    class="input"
                    type="text"
                    autocomplete="off"
                    spellcheck="false"
                    :placeholder="domain.name"
                    :aria-describedby="`${id}-confirm-hint`"
                />
                <p :id="`${id}-confirm-hint`" class="hint">Type <strong>{{ domain.name }}</strong> exactly as shown.</p>
            </template>
            <p v-else class="hint">Afterwards this page lists the DNS records to clean up.</p>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary danger" :disabled="!matches || isSubmitting || !!blocker">
                {{ isSubmitting ? 'Removing…' : 'Remove domain' }}
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import type { DependentSubdomain } from '@/domain-removal';
import type { Domain } from '@/openapi-client-generated';
import { useModalPresence } from '@/shared/modal-state';

import ApiErrorNotice from './api-error-notice.vue';

const props = defineProps<{
    appId: string;
    domain: Domain;
    /** Verified subdomains that rely on this domain's bounce or tracking domain. */
    subdomains: DependentSubdomain[];
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
    /** The server refused because the app's SMTP From policy sends as this domain. */
    policyBlocked: boolean;
    /** Why the request cannot be sent right now, for example an ended session. */
    blocker: string | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [];
}>();

useModalPresence();
const id = useId();
const typed = ref('');
/** Only a verified domain asks for its name; an unverified one never sent mail. */
const matches = computed(() => !props.domain.active || typed.value === props.domain.name);
const trackingName = computed(() => (props.domain.tracking_domain && props.domain.tracking_domain.state !== 'removed' ? props.domain.tracking_domain.name : null));

function cancel() {
    if (props.isSubmitting) return;
    emit('close');
}

function submit() {
    if (!matches.value || props.isSubmitting || props.blocker) return;
    emit('submit');
}
</script>

<style lang="scss" scoped>
.target dd {
    overflow-wrap: anywhere;
}

.modal-body {
    width: min(520px, calc(100vw - 72px));

    label {
        margin-top: 4px;
        color: var(--text);
        font-weight: 600;
    }
}

.notice ul {
    display: flex;
    flex-direction: column;
    gap: 6px;
}

.blocker {
    color: var(--danger);
    font-size: 13px;
    font-weight: 600;
}
</style>
