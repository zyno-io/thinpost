<template>
    <VfModal
        role="dialog"
        aria-modal="true"
        :aria-labelledby="`${id}-title`"
        class="tp-modal retry-policy-modal"
        @close="cancel"
        @form-submit="submit"
    >
        <template #header>
            <span :id="`${id}-title`">Edit retry schedule</span>
        </template>

        <div class="modal-body">
            <ApiErrorNotice v-if="error" :error="error" />

            <div class="field">
                <label :for="`${id}-attempts`">Maximum attempts</label>
                <input :id="`${id}-attempts`" ref="attemptsInput" v-model.trim="draft.maxAttempts" v-autofocus class="input" type="text" inputmode="numeric" autocomplete="off" :aria-invalid="!!shown.maxAttempts" @blur="touched.maxAttempts = true" />
                <p v-if="shown.maxAttempts" class="error-text">{{ shown.maxAttempts }}</p>
                <p v-else class="hint">1 to {{ formatCount(POLICY_LIMITS.attempts.max) }}, counting the first try.</p>
            </div>
            <div class="field">
                <label :for="`${id}-initial`">Initial delay (minutes)</label>
                <input :id="`${id}-initial`" v-model.trim="draft.initialMinutes" class="input" type="text" inputmode="decimal" autocomplete="off" :aria-invalid="!!shown.initial" @blur="touched.initial = true" />
                <p v-if="shown.initial" class="error-text">{{ shown.initial }}</p>
                <p v-else class="hint">{{ preview(draft.initialMinutes) }} · 1 min to 1 d. Each retry waits longer, with a little randomness.</p>
            </div>
            <div class="field">
                <label :for="`${id}-max-delay`">Maximum delay (minutes)</label>
                <input :id="`${id}-max-delay`" v-model.trim="draft.maxDelayMinutes" class="input" type="text" inputmode="decimal" autocomplete="off" :aria-invalid="!!shown.maxDelay" @blur="touched.maxDelay = true" />
                <p v-if="shown.maxDelay" class="error-text">{{ shown.maxDelay }}</p>
                <p v-else class="hint">{{ preview(draft.maxDelayMinutes) }} · at least the initial delay, up to 1 d.</p>
            </div>
            <div class="field">
                <label :for="`${id}-max-age`">Maximum age (minutes)</label>
                <input :id="`${id}-max-age`" v-model.trim="draft.maxAgeMinutes" class="input" type="text" inputmode="decimal" autocomplete="off" :aria-invalid="!!shown.maxAge" @blur="touched.maxAge = true" />
                <p v-if="shown.maxAge" class="error-text">{{ shown.maxAge }}</p>
                <p v-else class="hint">{{ preview(draft.maxAgeMinutes) }} · counted from the first attempt, 1 h to 30 d.</p>
            </div>

            <ul class="effects">
                <li>Only affects retries scheduled from now on. Existing retry times are unchanged.</li>
                <li>Applies to all apps immediately. The change is recorded in the audit log.</li>
            </ul>

            <label class="acknowledge">
                <input v-model="confirmed" type="checkbox" />
                <span>Change the retry schedule for all apps.</span>
            </label>

            <p v-if="isUnchanged" class="hint" role="status">This is already the current schedule.</p>
            <p v-if="blocker" class="blocker" role="status">{{ blocker }}</p>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary" :disabled="!canSubmit">{{ isSubmitting ? 'Saving…' : 'Save schedule' }}</button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, nextTick, reactive, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import { draftFromPolicy, formatMs, POLICY_LIMITS, samePolicy, validatePolicyDraft, type PolicyFields } from '@/operations';
import type { RetryPolicy } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import { useModalPresence } from '@/shared/modal-state';
import { formatCount } from '@/submission-rate';

const props = defineProps<{
    current: RetryPolicy;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
    /** Why the request cannot be sent right now, for example closed mail intake or an ended session. */
    blocker: string | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [fields: PolicyFields];
}>();

useModalPresence();
const id = useId();

const draft = reactive(draftFromPolicy(props.current));
const touched = reactive({ maxAttempts: false, initial: false, maxDelay: false, maxAge: false });
const confirmed = ref(false);
const attemptsInput = ref<HTMLInputElement | null>(null);

const validation = computed(() => validatePolicyDraft(draft));
/** Errors appear once a field was visited, or after a submit attempt. */
const shown = computed(() => ({
    maxAttempts: touched.maxAttempts ? validation.value.errors.maxAttempts : undefined,
    initial: touched.initial ? validation.value.errors.initial : undefined,
    maxDelay: touched.maxDelay ? validation.value.errors.maxDelay : undefined,
    maxAge: touched.maxAge ? validation.value.errors.maxAge : undefined
}));
const isUnchanged = computed(() => !!validation.value.value && samePolicy(validation.value.value, props.current));
const canSubmit = computed(() => !props.isSubmitting && !props.blocker && !isUnchanged.value && confirmed.value);

/** Minutes typed by the user as days, hours and minutes; blank while the value is not a number. */
function preview(minutes: string): string {
    const value = Number(minutes);
    return /^\d+(\.\d{1,3})?$/.test(minutes.trim()) && Number.isFinite(value) ? formatMs(Math.round(value * 60_000)) : '—';
}

function cancel() {
    if (props.isSubmitting) return;
    emit('close');
}

async function submit() {
    if (!canSubmit.value) return;
    touched.maxAttempts = touched.initial = touched.maxDelay = touched.maxAge = true;
    if (!validation.value.value) {
        await nextTick();
        attemptsInput.value?.focus();
        return;
    }
    emit('submit', { ...validation.value.value });
}
</script>

<style lang="scss" scoped>
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

.hint,
.effects {
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
