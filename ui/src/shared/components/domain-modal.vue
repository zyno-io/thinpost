<template>
    <VfModal class="tp-modal" role="dialog" aria-modal="true" :aria-labelledby="`${id}-title`" close-on-mask-click @close="emit('close')" @form-submit="submit">
        <template #header>
            <span :id="`${id}-title`">Add sending domain</span>
        </template>

        <div class="modal-body">
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
                placeholder="mail.example.com"
                required
                :aria-invalid="!!result.error && touched"
                :aria-describedby="`${id}-hint`"
                @blur="touched = true"
            />
            <p :id="`${id}-hint`" class="hint" :class="{ invalid: result.error && touched }">
                {{ result.error && touched ? result.error : 'Use the domain as plain letters and digits. For international names, use the xn-- form.' }}
            </p>
        </div>

        <template #footer>
            <button type="button" class="btn" @click="emit('close')">Cancel</button>
            <button type="submit" class="btn primary" :disabled="isSubmitting">
                {{ isSubmitting ? 'Working…' : 'Get TXT record' }}
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import { useModalPresence } from '@/shared/modal-state';
import { normalizeDomain } from '@/validation';

import ApiErrorNotice from './api-error-notice.vue';

const props = defineProps<{
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
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
    touched.value = true;
    const { domain } = result.value;
    if (!domain || props.isSubmitting) return;
    emit('submit', domain);
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
