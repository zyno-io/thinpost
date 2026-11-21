<template>
    <VfModal class="tp-modal" role="dialog" aria-modal="true" :aria-labelledby="`${id}-title`" close-on-mask-click @close="emit('close')" @form-submit="submit">
        <template #header>
            <span :id="`${id}-title`">{{ current ? 'Change tracking domain' : 'Add tracking domain' }}</span>
        </template>

        <div class="modal-body">
            <ApiErrorNotice v-if="error" :error="error" />

            <label :for="`${id}-name`">Tracking domain</label>
            <input
                :id="`${id}-name`"
                v-model="value"
                v-autofocus
                class="input"
                type="text"
                inputmode="url"
                autocomplete="off"
                autocapitalize="off"
                spellcheck="false"
                :placeholder="suggestTrackingName(sender)"
                required
                :aria-invalid="!!result.error && touched"
                :aria-describedby="`${id}-hint`"
                @blur="touched = true"
            />
            <p :id="`${id}-hint`" class="hint" :class="{ invalid: result.error && touched }">
                {{ result.error && touched ? result.error : `A subdomain of ${sender} that you don't use for anything else.` }}
            </p>
            <p class="hint">
                After you save, you add one DNS record for it, and your Thinpost administrator finishes setting up the address. Until it works, links keep using Thinpost's standard tracking
                address.
                <template v-if="current">The current one, {{ current }}, is replaced.</template>
            </p>
        </div>

        <template #footer>
            <button type="button" class="btn" @click="emit('close')">Cancel</button>
            <button type="submit" class="btn primary" :disabled="isSubmitting">
                {{ isSubmitting ? 'Saving…' : 'Save' }}
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import { useModalPresence } from '@/shared/modal-state';
import { suggestTrackingName, validateTrackingName } from '@/tracking-domains';

import ApiErrorNotice from './api-error-notice.vue';

const props = defineProps<{
    /** The sending domain the tracking domain belongs to. */
    sender: string;
    /** The tracking domain being replaced, if any. */
    current?: string | null;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [name: string];
}>();

useModalPresence();
const id = useId();
const value = ref(props.current ?? '');
const touched = ref(false);
const result = computed(() => validateTrackingName(value.value, props.sender));

function submit() {
    touched.value = true;
    const { name } = result.value;
    if (!name || props.isSubmitting) return;
    emit('submit', name);
}
</script>

<style lang="scss" scoped>
.modal-body {
    gap: 8px;
    width: min(480px, calc(100vw - 72px));

    label {
        margin-top: 4px;
        color: var(--text);
        font-weight: 600;
    }

    .hint.invalid {
        color: var(--danger);
    }
}
</style>
