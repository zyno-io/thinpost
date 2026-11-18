<template>
    <VfModal
        role="dialog"
        aria-modal="true"
        :aria-labelledby="`${id}-title`"
        class="tp-modal inbound-limits-modal"
        @close="cancel"
        @form-submit="submit"
    >
        <template #header>
            <span :id="`${id}-title`">Inbound limits</span>
        </template>

        <div class="modal-body">
            <ApiErrorNotice v-if="error" :error="error" />

            <div class="field">
                <label :for="`${id}-daily`">Messages per day</label>
                <input
                    :id="`${id}-daily`"
                    ref="dailyInput"
                    v-model.trim="daily"
                    v-autofocus
                    class="input"
                    type="text"
                    inputmode="numeric"
                    autocomplete="off"
                    spellcheck="false"
                    :aria-invalid="showDailyError"
                    :aria-describedby="`${id}-daily-hint`"
                    @blur="dailyTouched = true"
                />
                <p :id="`${id}-daily-hint`" class="hint" :class="{ invalid: showDailyError }">
                    {{ showDailyError ? dailyResult.error : `1 to ${formatCount(MAX_INBOUND_DAILY)}. Counted per UTC day; mail over the limit is delayed by the sender and retried.` }}
                </p>
            </div>

            <div class="field">
                <label :for="`${id}-pending`">Messages waiting for your webhook</label>
                <input
                    :id="`${id}-pending`"
                    ref="pendingInput"
                    v-model.trim="pending"
                    class="input"
                    type="text"
                    inputmode="numeric"
                    autocomplete="off"
                    spellcheck="false"
                    :aria-invalid="showPendingError"
                    :aria-describedby="`${id}-pending-hint`"
                    @blur="pendingTouched = true"
                />
                <p :id="`${id}-pending-hint`" class="hint" :class="{ invalid: showPendingError }">
                    {{ showPendingError ? pendingResult.error : `1 to ${formatCount(MAX_INBOUND_PENDING)}. When this many are waiting, new mail is delayed until your webhook catches up.` }}
                </p>
            </div>

            <p v-if="isUnchanged" class="hint" role="status">These match the current limits.</p>
            <p v-if="blocker" class="blocker" role="status">{{ blocker }}</p>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary" :disabled="!canSubmit">{{ isSubmitting ? 'Saving…' : 'Save limits' }}</button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, nextTick, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import { MAX_INBOUND_DAILY, MAX_INBOUND_PENDING, validateInboundLimit } from '@/inbound';
import type { InboundLimits } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import { useModalPresence } from '@/shared/modal-state';
import { formatCount } from '@/submission-rate';

const props = defineProps<{
    current: InboundLimits;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
    /** Why the request cannot be sent right now, for example an ended session. */
    blocker: string | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [limits: InboundLimits];
}>();

useModalPresence();
const id = useId();

const daily = ref(String(props.current.daily_limit));
const pending = ref(String(props.current.pending_limit));
const dailyTouched = ref(false);
const pendingTouched = ref(false);
const dailyInput = ref<HTMLInputElement | null>(null);
const pendingInput = ref<HTMLInputElement | null>(null);

const dailyResult = computed(() => validateInboundLimit(daily.value, MAX_INBOUND_DAILY, 'how many messages per day'));
const pendingResult = computed(() => validateInboundLimit(pending.value, MAX_INBOUND_PENDING, 'how many messages can wait'));
const showDailyError = computed(() => dailyTouched.value && !!dailyResult.value.error);
const showPendingError = computed(() => pendingTouched.value && !!pendingResult.value.error);
const isUnchanged = computed(() => dailyResult.value.value === props.current.daily_limit && pendingResult.value.value === props.current.pending_limit);
const canSubmit = computed(() => !props.isSubmitting && !props.blocker && !isUnchanged.value);

function cancel() {
    if (props.isSubmitting) return;
    emit('close');
}

async function submit() {
    if (!canSubmit.value) return;
    dailyTouched.value = pendingTouched.value = true;
    if (dailyResult.value.value === null) {
        await nextTick();
        dailyInput.value?.focus();
        return;
    }
    if (pendingResult.value.value === null) {
        await nextTick();
        pendingInput.value?.focus();
        return;
    }
    emit('submit', { daily_limit: dailyResult.value.value, pending_limit: pendingResult.value.value });
}
</script>

<style lang="scss" scoped>
.modal-body {
    gap: 12px;
    width: min(480px, calc(100vw - 72px));
}

.field {
    gap: 4px;

    label {
        color: var(--text);
        font-weight: 600;
    }

    .input {
        width: 160px;
    }
}

.hint {
    color: var(--text-3);
    font-size: 12px;

    &.invalid {
        color: var(--danger);
    }
}

.blocker {
    color: var(--danger);
    font-size: 13px;
    font-weight: 600;
}
</style>
