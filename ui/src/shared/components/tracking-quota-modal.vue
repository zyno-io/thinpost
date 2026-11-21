<template>
    <VfModal
        role="dialog"
        aria-modal="true"
        :aria-labelledby="`${id}-title`"
        class="tp-modal tracking-quota-modal"
        @close="cancel"
        @form-submit="submit"
    >
        <template #header>
            <span :id="`${id}-title`">Daily tracking limit</span>
        </template>

        <div class="modal-body">
            <dl class="target">
                <dt>App</dt>
                <dd>{{ appName }} <code>{{ appId }}</code></dd>
                <dt>Current limit</dt>
                <dd>{{ formatCount(current.daily_limit) }} events per UTC day, as of <time :datetime="loadedAt.toISOString()">{{ formatTime(loadedAt) }}</time></dd>
            </dl>

            <ApiErrorNotice v-if="error" :error="error" />

            <div class="field">
                <label :for="`${id}-limit`">Events per UTC day</label>
                <input
                    :id="`${id}-limit`"
                    ref="limitInput"
                    v-model.trim="limit"
                    v-autofocus
                    class="input"
                    type="text"
                    inputmode="numeric"
                    autocomplete="off"
                    spellcheck="false"
                    aria-required="true"
                    :aria-invalid="showError"
                    :aria-describedby="showError ? `${id}-hint ${id}-error` : `${id}-hint`"
                    @blur="touched = true"
                />
                <p :id="`${id}-hint`" class="hint">0 to {{ formatCount(MAX_DAILY_LIMIT) }}. Zero means no events are recorded.</p>
                <p v-if="showError" :id="`${id}-error`" class="error-text">{{ result.error }}</p>
            </div>

            <div v-if="result.value === 0" :id="`${id}-zero`" class="notice warning">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <p class="notice-body">No opens or clicks will be recorded for this app. Tracked links still work.</p>
            </div>

            <ul class="effects">
                <li>Takes effect immediately. Today's count is not reset. The change is recorded in the audit log.</li>
            </ul>

            <label class="acknowledge">
                <input v-model="confirmed" type="checkbox" />
                <span>Change the limit for “{{ appName }}”.</span>
            </label>

            <p v-if="isUnchanged" class="unchanged" role="status">This is already the current limit.</p>
            <p v-if="blocker" class="blocker" role="status">{{ blocker }}</p>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary" :disabled="!canSubmit">{{ isSubmitting ? 'Saving…' : 'Save limit' }}</button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, nextTick, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import type { TrackingQuota } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import { useModalPresence } from '@/shared/modal-state';
import { formatCount } from '@/submission-rate';
import { MAX_DAILY_LIMIT, validateDailyLimit } from '@/tracking';

const props = defineProps<{
    appId: string;
    appName: string;
    current: TrackingQuota;
    loadedAt: Date;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
    /** Why the request cannot be sent right now, for example an ended session. */
    blocker: string | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [dailyLimit: number];
}>();

useModalPresence();
const id = useId();

const limit = ref(String(props.current.daily_limit));
const touched = ref(false);
const confirmed = ref(false);
const limitInput = ref<HTMLInputElement | null>(null);

const result = computed(() => validateDailyLimit(limit.value));
const showError = computed(() => touched.value && !!result.value.error);
const isUnchanged = computed(() => result.value.value === props.current.daily_limit);
const canSubmit = computed(() => !props.isSubmitting && !props.blocker && !isUnchanged.value && confirmed.value);

const timeFormat = new Intl.DateTimeFormat(undefined, { timeStyle: 'medium' });
function formatTime(date: Date) {
    return timeFormat.format(date);
}

function cancel() {
    if (props.isSubmitting) return;
    emit('close');
}

async function submit() {
    if (!canSubmit.value) return;
    touched.value = true;
    if (result.value.value === null) {
        await nextTick();
        limitInput.value?.focus();
        return;
    }
    emit('submit', result.value.value);
}
</script>

<style lang="scss" scoped>
.target dd {
    overflow-wrap: anywhere;
}

.field {
    gap: 4px;

    label:not(:has(input[type='checkbox'])) {
        color: var(--text);
        font-weight: 600;
    }

    .input {
        width: 160px;
    }
}

.hint,
.effects,
.unchanged {
    color: var(--text-3);
    font-size: 12px;
}

.error-text {
    color: var(--danger);
    font-size: 12px;
}

.effects {
    display: flex;
    flex-direction: column;
    gap: 4px;
    margin: 0;
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
