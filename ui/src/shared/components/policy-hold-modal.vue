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
            <span :id="`${id}-title`">Retry this held delivery?</span>
        </template>

        <div class="modal-body">
            <dl class="target">
                <dt>App</dt>
                <dd>{{ appName }} <code>{{ appId }}</code></dd>
                <dt>Message ID</dt>
                <dd><code>{{ messageId }}</code></dd>
                <dt>Delivery ID</dt>
                <dd><code>{{ deliveryId }}</code></dd>
                <dt>Recipient</dt>
                <dd>
                    #{{ recipientIndex + 1 }} in the message<template v-if="recipient"> · {{ recipient }}</template>
                </dd>
            </dl>

            <div :id="`${id}-warning`" class="notice warning">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <div class="notice-body"><ul>
                    <li>
                        <strong>This only puts the delivery back in the queue. It doesn't override the recipient rules.</strong> Thinpost checks the
                        rules again before the next attempt. If the recipient is still not allowed, the delivery goes back on hold.
                    </li>
                    <li>This doesn't work if the delivery is no longer held or the app is paused. The retry is recorded in the audit log.</li>
                </ul></div>
            </div>

            <label class="acknowledge">
                <input v-model="acknowledged" v-autofocus type="checkbox" />
                <span>I understand the rules will be checked again and the delivery may go back on hold.</span>
            </label>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary" :disabled="!acknowledged || isSubmitting">
                {{ isSubmitting ? 'Retrying…' : 'Retry delivery' }}
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { ref, useId } from 'vue';

import { useModalPresence } from '@/shared/modal-state';

const props = defineProps<{
    appId: string;
    appName: string;
    messageId: string;
    deliveryId: string;
    recipientIndex: number;
    /** Envelope address of the delivery, when recorded. */
    recipient?: string | null;
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
.target {
    dt {
        color: var(--text-3);
    }

    dd {
        overflow-wrap: anywhere;
    }

    code {
        font-size: 12px;
    }
}

.notice ul {
    display: flex;
    flex-direction: column;
    gap: 6px;
    margin: 0;
    padding-left: 18px;
}

.acknowledge {
    align-items: flex-start;
}
</style>
