<template>
    <VfModal
        role="dialog"
        aria-modal="true"
        :aria-labelledby="`${id}-title`"
        :aria-describedby="`${id}-intro`"
        class="tp-modal count-limit-modal"
        @close="cancel"
        @form-submit="submit"
    >
        <template #header>
            <span :id="`${id}-title`">Change {{ config.noun }}</span>
        </template>

        <div class="modal-body">
            <dl class="target">
                <dt>App</dt>
                <dd>{{ appName }} <code>{{ appId }}</code></dd>
                <dt>Current setting</dt>
                <dd>
                    {{ summarizeLimit(config, current) }}, as of <time :datetime="loadedAt.toISOString()">{{ formatTime(loadedAt) }}</time>
                </dd>
            </dl>

            <p :id="`${id}-intro`" class="intro">
                <template v-if="config.kind === 'recipient'">Limits how many recipients this app can send to each UTC day, across all of its API keys.</template>
                <template v-else>Limits how many of this app's messages can be pending at once, across all of its API keys.</template>
            </p>

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

            <div v-if="mode === 'limited'" class="field">
                <label :for="`${id}-limit`">{{ config.fieldLabel }}</label>
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
                    :aria-describedby="showError ? `${id}-limit-hint ${id}-limit-error` : `${id}-limit-hint`"
                    @blur="touched = true"
                />
                <p :id="`${id}-limit-hint`" class="hint">1 to {{ formatCount(config.max) }}. Zero is set by blocking all new messages.</p>
                <p v-if="showError" :id="`${id}-limit-error`" class="error-text">{{ result.error }}</p>
            </div>

            <div v-else :id="`${id}-disable-warning`" class="notice warning">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <p class="notice-body">
                    <strong>Every new message will be rejected</strong> until a limit is set again. Messages already accepted are not affected.
                </p>
            </div>

            <ul class="effects">
                <li v-if="config.kind === 'recipient'">Takes effect immediately. Today's count is not reset. The change is recorded in the audit log.</li>
                <li v-else>Takes effect immediately. Messages already pending are not affected. The change is recorded in the audit log.</li>
            </ul>

            <label v-if="needsAcknowledgement" class="acknowledge">
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
import { computed, nextTick, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import { type CountLimit, type CountLimitConfig, isBlocked, summarizeLimit, validateCountLimit } from '@/count-limits';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import { useModalPresence } from '@/shared/modal-state';
import { formatCount } from '@/submission-rate';

const props = defineProps<{
    config: CountLimitConfig;
    appId: string;
    appName: string;
    current: CountLimit;
    loadedAt: Date;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
    /** Why the request cannot be sent right now, for example an ended session. */
    blocker: string | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [limit: number];
}>();

useModalPresence();
const id = useId();

const mode = ref<'limited' | 'disabled'>('limited');
const limit = ref(isBlocked(props.current) ? '' : String(props.current.limit));
const touched = ref(false);
const acknowledged = ref(false);
const limitInput = ref<HTMLInputElement | null>(null);

const result = computed(() => validateCountLimit(limit.value, props.config.max));
const showError = computed(() => touched.value && !!result.value.error);

/** The limit the form would save, or null while the input is invalid. */
const proposed = computed<number | null>(() => (mode.value === 'disabled' ? 0 : result.value.value));
const isUnchanged = computed(() => proposed.value === props.current.limit);
const needsAcknowledgement = computed(() => mode.value === 'disabled' && !isBlocked(props.current));
const canSubmit = computed(
    () => !props.isSubmitting && !props.blocker && !isUnchanged.value && (!needsAcknowledgement.value || acknowledged.value)
);

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
    if (mode.value === 'limited') {
        touched.value = true;
        if (result.value.error) {
            await nextTick();
            limitInput.value?.focus();
            return;
        }
    }
    if (proposed.value !== null) emit('submit', proposed.value);
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
