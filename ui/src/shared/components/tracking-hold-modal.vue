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
            <span :id="`${id}-title`">Mark delivery failed without sending it?</span>
        </template>

        <div class="modal-body">
            <dl class="target">
                <dt>App</dt>
                <dd>
                    {{ appName }} <code>{{ appId }}</code>
                    <span v-if="!appActive" class="tag">Paused</span>
                </dd>
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
                        <strong>Marks the delivery failed instead of trying to send it again.</strong> Failed is final, and you can't undo it from
                        this console.
                    </li>
                    <li>
                        <strong>Doesn't send the message.</strong> To reach the recipient, your app needs to send a new message. This also doesn't
                        apply to deliveries with an uncertain result.
                    </li>
                    <li>
                        This only works while the delivery is still held because tracking is unavailable. The failure, an audit log entry and a
                        <code>tracking_abandoned</code> webhook event are saved together.
                        <template v-if="!appActive">The app is paused, so the webhook event waits until it is resumed.</template>
                    </li>
                </ul></div>
            </div>

            <label class="acknowledge">
                <input v-model="acknowledged" v-autofocus type="checkbox" />
                <span>I understand this marks the delivery failed without sending it, and I can't undo it here.</span>
            </label>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary danger" :disabled="!acknowledged || isSubmitting">
                {{ isSubmitting ? 'Marking failed…' : 'Mark failed' }}
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
    appActive: boolean;
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
