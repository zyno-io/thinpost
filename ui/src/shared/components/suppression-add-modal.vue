<template>
    <VfModal
        class="tp-modal"
        role="dialog"
        aria-modal="true"
        :aria-labelledby="`${id}-title`"
        @close="emit('close')"
        @form-submit="submit"
    >
        <template #header>
            <span :id="`${id}-title`">Add to suppression list</span>
        </template>

        <div class="modal-body">
            <ApiErrorNotice v-if="error" :error="error" />

            <label :for="`${id}-addresses`">Addresses</label>
            <textarea
                :id="`${id}-addresses`"
                v-model="text"
                v-autofocus
                class="input addresses"
                rows="8"
                autocomplete="off"
                spellcheck="false"
                placeholder="One address per line"
                :aria-describedby="`${id}-count`"
            />
            <p :id="`${id}-count`" class="hint count" :class="{ invalid: !!problem }" role="status">{{ summary }}</p>
        </div>

        <template #footer>
            <button type="button" class="btn" @click="emit('close')">Cancel</button>
            <button type="submit" class="btn primary" :disabled="isSubmitting || !!problem || !parsed.valid.length">
                {{ isSubmitting ? 'Adding…' : 'Add' }}
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import { useModalPresence } from '@/shared/modal-state';
import { MAX_ADD, parseAddresses } from '@/suppressions';

import ApiErrorNotice from './api-error-notice.vue';

const props = defineProps<{
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [addresses: string[]];
}>();

useModalPresence();
const id = useId();
const text = ref('');
const parsed = computed(() => parseAddresses(text.value));
const problem = computed(() => {
    if (parsed.value.invalid > 0) return true;
    return parsed.value.valid.length > MAX_ADD;
});
const summary = computed(() => {
    const { valid, invalid } = parsed.value;
    if (!valid.length && !invalid) return `Up to ${MAX_ADD} at a time.`;
    const parts = [`${valid.length} valid`];
    if (invalid) parts.push(`${invalid} invalid`);
    if (valid.length > MAX_ADD) parts.push(`over the ${MAX_ADD} limit`);
    return parts.join(' · ');
});

function submit() {
    if (props.isSubmitting || problem.value || !parsed.value.valid.length) return;
    emit('submit', parsed.value.valid);
}
</script>

<style lang="scss" scoped>
.modal-body {
    gap: 8px;
    width: min(480px, calc(100vw - 72px));

    label {
        color: var(--text);
        font-weight: 600;
    }

    .addresses {
        min-height: 140px;
        resize: vertical;
        font-family: var(--font-mono, ui-monospace, monospace);
        font-size: 12.5px;
        white-space: pre;
    }

    .hint.invalid {
        color: var(--danger);
    }
}
</style>
