<template>
    <VfModal
        ref="modal"
        class="tp-modal"
        role="dialog"
        aria-modal="true"
        :aria-labelledby="`${id}-title`"
        close-on-mask-click
        @close="emit('close')"
        @form-submit="submit"
    >
        <template #header>
            <span :id="`${id}-title`">{{ title }}</span>
        </template>

        <div class="modal-body">
            <ApiErrorNotice v-if="error" :error="error" />

            <label :for="`${id}-name`">{{ label }}</label>
            <input
                :id="`${id}-name`"
                v-model="name"
                v-autofocus
                class="input"
                type="text"
                autocomplete="off"
                spellcheck="false"
                required
                :aria-invalid="!!validationError && touched"
                :aria-describedby="`${id}-hint`"
                @blur="touched = true"
            />
            <p :id="`${id}-hint`" class="hint" :class="{ invalid: validationError && touched }">
                {{ validationError && touched ? validationError : hint }}
            </p>
            <slot />
        </div>

        <template #footer>
            <button type="button" class="btn" @click="emit('close')">Cancel</button>
            <button type="submit" class="btn primary" :disabled="isSubmitting">
                {{ isSubmitting ? 'Saving…' : submitLabel }}
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import { useModalPresence } from '@/shared/modal-state';
import { validateName } from '@/validation';

import ApiErrorNotice from './api-error-notice.vue';

const props = defineProps<{
    title: string;
    label: string;
    hint: string;
    submitLabel: string;
    /** Pre-fills the field (for example the current name when renaming). */
    initialName?: string;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [name: string];
}>();

useModalPresence();
const id = useId();
const name = ref(props.initialName ?? '');
const touched = ref(false);
const validationError = computed(() => validateName(name.value));

function submit() {
    touched.value = true;
    if (validationError.value || props.isSubmitting) return;
    emit('submit', name.value);
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
