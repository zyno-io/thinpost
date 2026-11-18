<template>
    <VfModal
        class="tp-modal"
        role="alertdialog"
        aria-modal="true"
        :aria-labelledby="`${id}-title`"
        :aria-describedby="`${id}-warning`"
        :data-status-action="action"
        @close="cancel"
        @form-submit="submit"
    >
        <template #header>
            <span :id="`${id}-title`">{{ action === 'pause' ? 'Pause app?' : 'Resume app?' }}</span>
        </template>

        <div class="modal-body">
            <dl class="target">
                <dt>App</dt>
                <dd>{{ appName }} <code>{{ appId }}</code></dd>
                <dt>Status</dt>
                <dd>{{ shownActive ? 'Active' : 'Paused' }} as of <time :datetime="loadedAt.toISOString()">{{ formatTime(loadedAt) }}</time></dd>
            </dl>

            <div v-if="action === 'pause'" :id="`${id}-warning`" class="notice warning">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <ul class="notice-body">
                    <li>
                        <strong>Stops:</strong> new messages being accepted from this app, queued messages being sent, and webhook events being delivered.
                    </li>
                    <li>
                        <strong>Sends already under way may still finish;</strong> they can't be recalled.
                    </li>
                    <li>API keys and queued messages are kept. The pause is recorded in the audit log.</li>
                </ul>
            </div>

            <div v-else :id="`${id}-warning`" class="notice warning">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <ul class="notice-body">
                    <li>
                        <strong>API keys that haven't been revoked work again, and queued messages can be sent.</strong> Sending limits, recipient
                        rules and domain verification still apply.
                    </li>
                    <li>Queued messages and webhook events start going out right away and can't be recalled. Review queued messages first.</li>
                    <li>The change is recorded in the audit log.</li>
                </ul>
            </div>

            <p v-if="blocker" class="blocker" role="status">{{ blocker }}</p>

            <label class="acknowledge">
                <input v-model="acknowledged" v-autofocus type="checkbox" />
                <span v-if="action === 'pause'">I understand that sends already under way may still finish, and pausing can't recall them.</span>
                <span v-else>I understand that existing API keys and queued messages become active again.</span>
            </label>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary" :class="{ danger: action === 'pause' }" :disabled="!acknowledged || isSubmitting || !!blocker">
                <template v-if="action === 'pause'">{{ isSubmitting ? 'Pausing…' : 'Pause app' }}</template>
                <template v-else>{{ isSubmitting ? 'Resuming…' : 'Resume app' }}</template>
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { ref, useId } from 'vue';

import { useModalPresence } from '@/shared/modal-state';

export type AppStatusAction = 'pause' | 'resume';

const props = defineProps<{
    action: AppStatusAction;
    appId: string;
    appName: string;
    shownActive: boolean;
    loadedAt: Date;
    isSubmitting: boolean;
    /** Why the request cannot be sent right now, for example an ended session. */
    blocker: string | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [];
}>();

useModalPresence();
const id = useId();
const acknowledged = ref(false);

const timeFormat = new Intl.DateTimeFormat(undefined, { timeStyle: 'medium' });
function formatTime(date: Date) {
    return timeFormat.format(date);
}

function cancel() {
    if (props.isSubmitting) return;
    emit('close');
}

function submit() {
    if (!acknowledged.value || props.isSubmitting || props.blocker) return;
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

.blocker {
    color: var(--danger);
    font-size: 13px;
    font-weight: 600;
}

.acknowledge {
    align-items: flex-start;
}
</style>
