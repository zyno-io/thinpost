<template>
    <VfModal
        class="tp-modal"
        role="alertdialog"
        aria-modal="true"
        :aria-labelledby="`${id}-title`"
        :aria-describedby="`${id}-warning`"
        @close="cancel"
        @form-submit="submit"
    >
        <template #header>
            <span :id="`${id}-title`">Send this webhook event again?</span>
        </template>

        <div class="modal-body">
            <dl class="target">
                <dt>Event ID</dt>
                <dd><code>{{ eventId }}</code></dd>
                <dt>Webhook ID</dt>
                <dd><code>{{ subscriptionId }}</code></dd>
                <template v-if="endpoint">
                    <dt>Webhook</dt>
                    <dd><code class="endpoint">{{ endpoint }}</code></dd>
                </template>
            </dl>

            <div :id="`${id}-warning`" class="notice warning">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <ul class="notice-body">
                    <li>
                        <strong>Your receiver may get this event more than once.</strong> The same event ID is sent again, and an earlier attempt may already have arrived. Your receiver should ignore repeats.
                    </li>
                    <li>The attempt count starts over and automatic retries begin again. Earlier attempts stay in the history.</li>
                    <li>This only works if the event is still failed and both the app and the webhook are enabled.</li>
                </ul>
            </div>

            <label class="acknowledge">
                <input v-model="acknowledged" v-autofocus type="checkbox" />
                <span>I understand my receiver may get this event more than once.</span>
            </label>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary" :disabled="!acknowledged || isSubmitting">
                {{ isSubmitting ? 'Sending…' : 'Send again' }}
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { ref, useId } from 'vue';

import { useModalPresence } from '@/shared/modal-state';

const props = defineProps<{
    eventId: string;
    subscriptionId: string;
    /** The endpoint, when the endpoint list is available; never entered here. */
    endpoint: string | null;
    isSubmitting: boolean;
}>();

const emit = defineEmits<{
    close: [];
    submit: [];
}>();

useModalPresence();
const id = useId();
const acknowledged = ref(false);

function cancel() {
    if (props.isSubmitting) return;
    emit('close');
}

function submit() {
    if (!acknowledged.value || props.isSubmitting) return;
    emit('submit');
}
</script>

<style lang="scss" scoped>
.target dd {
    overflow-wrap: anywhere;
}

.notice ul {
    display: flex;
    flex-direction: column;
    gap: 6px;
}

.acknowledge {
    align-items: flex-start;
}
</style>
