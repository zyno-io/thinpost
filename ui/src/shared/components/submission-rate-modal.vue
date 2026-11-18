<template>
    <VfModal
        role="dialog"
        aria-modal="true"
        :aria-labelledby="`${id}-title`"
        :aria-describedby="`${id}-intro`"
        class="tp-modal submission-rate-modal"
        @close="cancel"
        @form-submit="submit"
    >
        <template #header>
            <span :id="`${id}-title`">Change sending limit</span>
        </template>

        <div class="modal-body">
            <dl class="target">
                <dt>App</dt>
                <dd>{{ appName }} <code>{{ appId }}</code></dd>
                <dt>Current setting</dt>
                <dd>
                    {{ currentSummary }}, as of <time :datetime="loadedAt.toISOString()">{{ formatTime(loadedAt) }}</time>
                </dd>
            </dl>

            <p :id="`${id}-intro`" class="intro">Limits how fast all of this app's API keys can send messages. Each message uses one token.</p>

            <ApiErrorNotice v-if="error" :error="error" />

            <fieldset class="mode">
                <legend>New messages</legend>
                <label>
                    <input v-model="mode" type="radio" :name="`${id}-mode`" value="limited" />
                    <span>Allow, up to a limit</span>
                </label>
                <label>
                    <input v-model="mode" type="radio" :name="`${id}-mode`" value="disabled" />
                    <span>Block all new messages</span>
                </label>
            </fieldset>

            <template v-if="mode === 'limited'">
                <div class="field">
                    <label :for="`${id}-per-minute`">Messages per minute</label>
                    <input
                        :id="`${id}-per-minute`"
                        ref="perMinuteInput"
                        v-model.trim="perMinute"
                        v-autofocus
                        class="input"
                        type="text"
                        inputmode="numeric"
                        autocomplete="off"
                        spellcheck="false"
                        aria-required="true"
                        :aria-invalid="showPerMinuteError"
                        :aria-describedby="describedBy('per-minute', showPerMinuteError)"
                        @blur="touched.perMinute = true"
                    />
                    <p :id="`${id}-per-minute-hint`" class="hint">How many tokens come back each minute, 1 to {{ formatCount(MAX_PER_MINUTE) }}.</p>
                    <p v-if="showPerMinuteError" :id="`${id}-per-minute-error`" class="error-text">{{ perMinuteResult.error }}</p>
                </div>

                <div class="field">
                    <label :for="`${id}-burst`">Burst (messages)</label>
                    <input
                        :id="`${id}-burst`"
                        ref="burstInput"
                        v-model.trim="burst"
                        class="input"
                        type="text"
                        inputmode="numeric"
                        autocomplete="off"
                        spellcheck="false"
                        aria-required="true"
                        :aria-invalid="showBurstError"
                        :aria-describedby="describedBy('burst', showBurstError)"
                        @blur="touched.burst = true"
                    />
                    <p :id="`${id}-burst-hint`" class="hint">Most messages that can be sent at once, 1 to {{ formatCount(MAX_BURST) }}.</p>
                    <p v-if="showBurstError" :id="`${id}-burst-error`" class="error-text">{{ burstResult.error }}</p>
                </div>

                <p v-if="proposed && !isDisabled(proposed)" class="summary" aria-live="polite">
                    Up to {{ formatCount(proposed.burst) }} message{{ proposed.burst === 1 ? '' : 's' }} at once, then
                    {{ describeSustained(proposed.per_minute) }}. An empty bucket takes {{ describeRefill(proposed) }} to refill.
                </p>

                <div v-if="proposed && proposed.burst < MAX_RESEND_BATCH" class="notice warning" role="status">
                    <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                    <p class="notice-body">
                        Resend batches of more than {{ formatCount(proposed.burst) }} message{{ proposed.burst === 1 ? '' : 's' }} will always be rejected.
                    </p>
                </div>
            </template>

            <div v-else :id="`${id}-disable-warning`" class="notice warning">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <p class="notice-body">
                    <strong>Every new message will be rejected</strong> until a limit is set again. Messages already accepted are not affected.
                </p>
            </div>

            <ul class="effects">
                <li v-if="isDisabled(current)">Allowing messages again starts with a full burst.</li>
                <li>Takes effect immediately. The change is recorded in the audit log.</li>
            </ul>

            <label v-if="mode === 'disabled' && !isDisabled(current)" class="acknowledge">
                <input v-model="acknowledged" type="checkbox" :aria-describedby="`${id}-disable-warning`" />
                <span>I understand that all new messages from this app will be rejected.</span>
            </label>

            <p v-if="isUnchanged" class="unchanged" role="status">This is already the current setting.</p>
            <p v-if="blocker" class="blocker" role="status">{{ blocker }}</p>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary" :class="{ danger: mode === 'disabled' }" :disabled="!canSubmit">
                <template v-if="isSubmitting">Saving…</template>
                <template v-else>{{ mode === 'disabled' ? 'Block new messages' : 'Save limit' }}</template>
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, nextTick, reactive, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import type { SubmissionRate } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import { useModalPresence } from '@/shared/modal-state';
import {
    describeRefill,
    describeSustained,
    formatCount,
    isDisabled,
    MAX_BURST,
    MAX_PER_MINUTE,
    MAX_RESEND_BATCH,
    validateBurst,
    validatePerMinute
} from '@/submission-rate';

const props = defineProps<{
    appId: string;
    appName: string;
    current: SubmissionRate;
    loadedAt: Date;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
    /** Why the request cannot be sent right now, for example an ended session. */
    blocker: string | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [rate: SubmissionRate];
}>();

useModalPresence();
const id = useId();

const mode = ref<'limited' | 'disabled'>('limited');
const perMinute = ref(isDisabled(props.current) ? '' : String(props.current.per_minute));
const burst = ref(isDisabled(props.current) ? '' : String(props.current.burst));
const touched = reactive({ perMinute: false, burst: false });
const acknowledged = ref(false);
const perMinuteInput = ref<HTMLInputElement | null>(null);
const burstInput = ref<HTMLInputElement | null>(null);

const perMinuteResult = computed(() => validatePerMinute(perMinute.value));
const burstResult = computed(() => validateBurst(burst.value));
const showPerMinuteError = computed(() => touched.perMinute && !!perMinuteResult.value.error);
const showBurstError = computed(() => touched.burst && !!burstResult.value.error);

/** The setting the form would save, or null while the inputs are invalid. */
const proposed = computed<SubmissionRate | null>(() => {
    if (mode.value === 'disabled') return { per_minute: 0, burst: 0 };
    const rate = perMinuteResult.value.value;
    const size = burstResult.value.value;
    return rate !== null && size !== null ? { per_minute: rate, burst: size } : null;
});

const isUnchanged = computed(
    () => !!proposed.value && proposed.value.per_minute === props.current.per_minute && proposed.value.burst === props.current.burst
);

const needsAcknowledgement = computed(() => mode.value === 'disabled' && !isDisabled(props.current));

const canSubmit = computed(
    () => !props.isSubmitting && !props.blocker && !isUnchanged.value && (!needsAcknowledgement.value || acknowledged.value)
);

const currentSummary = computed(() =>
    isDisabled(props.current)
        ? 'new messages blocked'
        : `${formatCount(props.current.per_minute)} per minute, burst of ${formatCount(props.current.burst)}`
);

const timeFormat = new Intl.DateTimeFormat(undefined, { timeStyle: 'medium' });
function formatTime(date: Date) {
    return timeFormat.format(date);
}

function describedBy(field: 'per-minute' | 'burst', hasError: boolean) {
    return hasError ? `${id}-${field}-hint ${id}-${field}-error` : `${id}-${field}-hint`;
}

function cancel() {
    if (props.isSubmitting) return;
    emit('close');
}

async function submit() {
    if (!canSubmit.value) return;
    if (mode.value === 'limited') {
        touched.perMinute = true;
        touched.burst = true;
        if (perMinuteResult.value.error || burstResult.value.error) {
            await nextTick();
            (perMinuteResult.value.error ? perMinuteInput : burstInput).value?.focus();
            return;
        }
    }
    if (proposed.value) emit('submit', { ...proposed.value });
}
</script>

<style lang="scss" scoped>
.target dd {
    overflow-wrap: anywhere;
}

.mode {
    display: flex;
    flex-wrap: wrap;
    gap: 8px 20px;
    margin: 0;
    padding: 0;
    border: 0;

    legend {
        margin-bottom: 8px;
        padding: 0;
        color: var(--text);
        font-weight: 600;
    }

    label {
        display: flex;
        align-items: center;
        gap: 8px;
        cursor: pointer;
    }
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

.summary {
    padding: 12px;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-2);
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
