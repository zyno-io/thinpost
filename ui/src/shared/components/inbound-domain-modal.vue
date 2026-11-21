<template>
    <VfModal class="tp-modal" role="dialog" aria-modal="true" :aria-labelledby="`${id}-title`" close-on-mask-click @close="emit('close')" @form-submit="submit">
        <template #header>
            <span :id="`${id}-title`">{{ challenge ? `DNS records for ${challenge.domain}` : 'Add receiving domain' }}</span>
        </template>

        <div v-if="challenge" class="modal-body records">
            <p>Add the TXT record at your DNS provider and choose Verify. Publish the MX record only after the domain is verified. This is the only time the TXT value is shown.</p>
            <section aria-label="TXT record">
                <h3>1. TXT record, to prove you own the domain</h3>
                <CopyField label="Record name" :value="challenge.txtName" />
                <CopyField label="Record value" :value="challenge.txtValue" />
            </section>
            <section aria-label="MX record">
                <h3>2. MX record, so mail for the domain reaches Thinpost</h3>
                <template v-if="challenge.mxHost">
                    <CopyField label="Record name" :value="challenge.domain" />
                    <CopyField label="Mail server" :value="challenge.mxHost" />
                    <p class="hint" data-testid="mx-note">
                        Until the domain is verified, Thinpost refuses mail for it and senders get it back. Once it is verified but your application has no inbound mail webhook, senders keep trying for a few days. If the domain has other MX
                        records, only the mail sent to this server arrives here.
                    </p>
                </template>
                <p v-else-if="challenge.mxHostStatus === 'dns_unconfirmed'" class="hint" data-testid="modal-no-mx-host">
                    Thinpost can't confirm the mail server's name yet, so there is no MX record to show. This usually clears up on its own once DNS catches up. Try again in a few minutes, or ask your Thinpost administrator.
                </p>
                <p v-else class="hint" data-testid="modal-no-mx-host">The mail server hasn't connected yet, so there is no MX record to create. Try again in a few minutes, or ask your Thinpost administrator.</p>
            </section>
        </div>

        <div v-else class="modal-body">
            <ApiErrorNotice v-if="error" :error="error" />

            <label :for="`${id}-domain`">Domain</label>
            <input
                :id="`${id}-domain`"
                v-model="value"
                v-autofocus
                class="input"
                type="text"
                inputmode="url"
                autocomplete="off"
                autocapitalize="off"
                spellcheck="false"
                placeholder="inbound.example.com"
                required
                :aria-invalid="!!result.error && touched"
                :aria-describedby="`${id}-hint`"
                @blur="touched = true"
            />
            <p :id="`${id}-hint`" class="hint" :class="{ invalid: result.error && touched }">
                {{ result.error && touched ? result.error : 'The domain that will receive mail, for example a subdomain used only for replies. Use punycode (xn--…) for international names.' }}
            </p>
        </div>

        <template #footer>
            <template v-if="challenge">
                <button v-autofocus type="submit" class="btn primary">Done</button>
            </template>
            <template v-else>
                <button type="button" class="btn" @click="emit('close')">Cancel</button>
                <button type="submit" class="btn primary" :disabled="isSubmitting">
                    {{ isSubmitting ? 'Working…' : 'Get DNS records' }}
                </button>
            </template>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import type { InboundChallenge } from '@/inbound';
import { useModalPresence } from '@/shared/modal-state';
import { normalizeDomain } from '@/validation';

import ApiErrorNotice from './api-error-notice.vue';
import CopyField from './copy-field.vue';

const props = defineProps<{
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
    /** Set once the records exist; the modal then shows them instead of the form. */
    challenge: InboundChallenge | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [domain: string];
}>();

useModalPresence();
const id = useId();
const value = ref('');
const touched = ref(false);
const result = computed(() => normalizeDomain(value.value));

function submit() {
    if (props.challenge) {
        emit('close');
        return;
    }
    touched.value = true;
    const { domain } = result.value;
    if (!domain || props.isSubmitting) return;
    emit('submit', domain);
}
</script>

<style lang="scss" scoped>
.modal-body {
    gap: 8px;
    width: min(520px, calc(100vw - 72px));

    label {
        margin-top: 4px;
        color: var(--text);
        font-weight: 600;
    }

    .hint.invalid {
        color: var(--danger);
    }
}

.records section {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding-top: 8px;

    h3 {
        font-size: 14px;
        font-weight: 600;
    }
}
</style>
