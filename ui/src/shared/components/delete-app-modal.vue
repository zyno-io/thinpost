<template>
    <VfModal
        class="tp-modal"
        role="alertdialog"
        aria-modal="true"
        :aria-labelledby="`${id}-title`"
        :aria-describedby="`${id}-warning`"
        data-delete-app
        @close="cancel"
        @form-submit="submit"
    >
        <template #header>
            <span :id="`${id}-title`">Delete app?</span>
        </template>

        <div class="modal-body">
            <dl class="target">
                <dt>App</dt>
                <dd>{{ appName }} <code>{{ appId }}</code></dd>
            </dl>

            <div :id="`${id}-warning`" class="notice warning">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <ul class="notice-body">
                    <li><strong>The app is removed from this console</strong> and can't be brought back here.</li>
                    <li>Its API keys and SMTP credentials stop working right away. Apps that still use them will be refused.</li>
                    <li>Mail that hasn't been sent yet won't be sent, and held messages can't be released. Sends already under way may still finish.</li>
                    <li>Its receiving domains are released, so another app can use them.</li>
                    <li>Its messages and history are kept, not erased. Deleting doesn't remove them.</li>
                </ul>
            </div>

            <ApiErrorNotice v-if="error" :error="error" />
            <p v-if="blocker" class="blocker" role="status">{{ blocker }}</p>

            <label :for="`${id}-confirm`">Type the app name to confirm</label>
            <input
                :id="`${id}-confirm`"
                v-model="typed"
                v-autofocus
                class="input"
                type="text"
                autocomplete="off"
                spellcheck="false"
                :placeholder="appName"
                :aria-describedby="`${id}-confirm-hint`"
            />
            <p :id="`${id}-confirm-hint`" class="hint">Type <strong>{{ appName }}</strong> exactly as shown.</p>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary danger" :disabled="!matches || isSubmitting || !!blocker">
                {{ isSubmitting ? 'Deleting…' : 'Delete app' }}
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import { useModalPresence } from '@/shared/modal-state';

import ApiErrorNotice from './api-error-notice.vue';

const props = defineProps<{
    appId: string;
    appName: string;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
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
const matches = computed(() => typed.value === props.appName);

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
