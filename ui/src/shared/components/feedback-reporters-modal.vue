<template>
    <VfModal role="dialog" aria-modal="true" :aria-labelledby="`${id}-title`" class="tp-modal" @close="cancel" @form-submit="submit">
        <template #header>
            <span :id="`${id}-title`">Reporter domains</span>
        </template>

        <div class="modal-body">
            <ApiErrorNotice v-if="error" :error="error" />

            <label :for="`${id}-domains`">Signing (DKIM) domains</label>
            <textarea
                :id="`${id}-domains`"
                v-model="text"
                v-autofocus
                class="input domains"
                rows="6"
                autocomplete="off"
                spellcheck="false"
                placeholder="One domain per line"
                :aria-describedby="`${id}-count`"
            />
            <p :id="`${id}-count`" class="hint" :class="{ invalid: !!parsed.problem }" role="status">{{ parsed.problem ?? summary }}</p>
            <p class="hint">A report is accepted only when its DKIM signature’s <code>d=</code> domain is a listed domain or one of its subdomains. An empty list accepts nothing.</p>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary" :disabled="isSubmitting || !!parsed.problem || unchanged">{{ isSubmitting ? 'Saving…' : 'Save' }}</button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import { MAX_REPORTERS, parseReporterDomains } from '@/feedback-loops';
import { useModalPresence } from '@/shared/modal-state';

import ApiErrorNotice from './api-error-notice.vue';

const props = defineProps<{
    current: readonly string[];
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [domains: string[]];
}>();

useModalPresence();
const id = useId();
const text = ref(props.current.join('\n'));
const parsed = computed(() => parseReporterDomains(text.value));
const unchanged = computed(() => parsed.value.domains.length === props.current.length && parsed.value.domains.every(d => props.current.includes(d)));
const summary = computed(() => `${parsed.value.domains.length} of ${MAX_REPORTERS} domains`);

function cancel() {
    if (!props.isSubmitting) emit('close');
}

function submit() {
    if (props.isSubmitting || parsed.value.problem || unchanged.value) return;
    emit('submit', parsed.value.domains);
}
</script>

<style lang="scss" scoped>
.domains {
    font-family: var(--font-mono, monospace);
    resize: vertical;
}

.hint {
    color: var(--text-3);
    font-size: 12px;

    &.invalid {
        color: var(--danger);
    }
}
</style>
