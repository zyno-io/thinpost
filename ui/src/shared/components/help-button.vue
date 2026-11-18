<template>
    <button
        ref="buttonEl"
        type="button"
        class="help-btn"
        :aria-label="`About ${topic}`"
        aria-haspopup="dialog"
        :aria-expanded="open"
        :title="`About ${topic}`"
        @click="open = true"
    >
        <i class="fa-solid fa-circle-question" aria-hidden="true" />
    </button>
    <Teleport to="body">
        <VfModal
            v-if="open"
            class="tp-modal"
            role="dialog"
            aria-modal="true"
            :aria-labelledby="`${id}-title`"
            close-on-mask-click
            @close="close"
            @form-submit="close"
        >
            <template #header>
                <span :id="`${id}-title`">{{ title }}</span>
            </template>
            <div class="modal-body help-body"><slot /></div>
            <template #footer>
                <button v-autofocus type="submit" class="btn primary">Close</button>
            </template>
        </VfModal>
    </Teleport>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, nextTick, onBeforeUnmount, ref, useId, watch } from 'vue';

import { openModalCount } from '@/shared/modal-state';

/**
 * A small "?" button that opens a modal with longer explanatory content (the default slot).
 * `topic` names the subject for the accessible label ("About <topic>") and is the modal title unless `heading` is set.
 */
const props = defineProps<{ topic: string; heading?: string }>();

const title = computed(() => {
    const text = props.heading ?? props.topic;
    return text.charAt(0).toUpperCase() + text.slice(1);
});

const id = useId();
const open = ref(false);
const buttonEl = ref<HTMLButtonElement>();

// Counted as an open modal only while shown, so the shell navigation is inert behind it.
watch(open, (isOpen, wasOpen) => {
    if (isOpen && !wasOpen) openModalCount.value++;
    else if (!isOpen && wasOpen) openModalCount.value--;
});
onBeforeUnmount(() => {
    if (open.value) openModalCount.value--;
});

function close() {
    open.value = false;
    void nextTick(() => buttonEl.value?.focus());
}
</script>

<style lang="scss" scoped>
.help-btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 20px;
    height: 20px;
    padding: 0;
    border: 0;
    border-radius: 50%;
    background: none;
    color: var(--text-3);
    font-size: 14px;
    line-height: 1;
    vertical-align: middle;
    cursor: pointer;

    &:hover {
        color: var(--accent);
    }

    &:focus-visible {
        outline: 2px solid var(--accent);
        outline-offset: 1px;
    }
}

.help-body {
    width: min(480px, calc(100vw - 72px));

    :deep(p) {
        margin: 0 0 8px;
        color: var(--text-2, var(--text));
    }

    :deep(p:last-child) {
        margin-bottom: 0;
    }

    :deep(ul) {
        margin: 0 0 8px;
        padding-left: 18px;
    }
}
</style>
