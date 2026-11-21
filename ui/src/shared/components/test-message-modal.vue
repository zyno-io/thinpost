<template>
    <VfModal class="tp-modal" role="dialog" aria-modal="true" :aria-labelledby="`${id}-title`" @close="emit('close')" @form-submit="submit">
        <template #header>
            <span :id="`${id}-title`">Send test message</span>
        </template>

        <div class="modal-body">
            <ApiErrorNotice v-if="error" :error="error" />
            <ApiErrorNotice v-if="domainsError" :error="domainsError" />

            <Loader v-if="domains === null && !domainsError" label="Loading sending domains" />

            <div v-else-if="domains && !domains.length" class="notice warning" role="note" data-testid="no-domains">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <div class="notice-body">
                    <p>
                        This app has no verified sending domain, so it has nothing to send from.
                        <RouterLink :to="{ name: 'app-domains', params: { appId } }" @click="emit('close')">Go to Sending domains</RouterLink>
                    </p>
                </div>
            </div>

            <template v-else-if="domains">
                <div class="notice" role="note">
                    <i class="fa-solid fa-circle-info" aria-hidden="true" />
                    <ul class="notice-body">
                        <li>
                            This sends a real message. It counts against this app's sending limit, daily recipient limit and pending-message limit,
                            unless you skip them below.
                        </li>
                        <li>This app's webhooks receive events for it, as for any message.</li>
                        <li>
                            Open and click tracking and the one-click unsubscribe link follow this app's settings. If a recipient clicks unsubscribe,
                            that address is suppressed for this app.
                        </li>
                    </ul>
                </div>

                <label :for="`${id}-local`">From</label>
                <div class="from-row">
                    <input
                        :id="`${id}-local`"
                        v-model="local"
                        v-autofocus
                        class="input"
                        type="text"
                        autocomplete="off"
                        autocapitalize="off"
                        spellcheck="false"
                        placeholder="test"
                        :aria-invalid="!!localProblem && localTouched"
                        :aria-describedby="`${id}-from-hint`"
                        @blur="localTouched = true"
                    />
                    <span class="at" aria-hidden="true">@</span>
                    <select v-model="domain" class="input" aria-label="Sending domain">
                        <option v-for="name in domains" :key="name" :value="name">{{ name }}</option>
                    </select>
                </div>
                <p :id="`${id}-from-hint`" class="hint" :class="{ invalid: !!localProblem && localTouched }">
                    {{ localProblem && localTouched ? localProblem : 'Only verified sending domains of this app are listed.' }}
                </p>

                <label :for="`${id}-name`">Display name <span class="muted">(optional)</span></label>
                <input :id="`${id}-name`" v-model="displayName" class="input" type="text" autocomplete="off" spellcheck="false" :aria-invalid="!!nameProblem" />
                <p v-if="nameProblem" class="hint invalid">{{ nameProblem }}</p>

                <label :for="`${id}-to`">To</label>
                <textarea
                    :id="`${id}-to`"
                    v-model="toText"
                    class="input recipients"
                    rows="3"
                    autocomplete="off"
                    spellcheck="false"
                    placeholder="One address per line"
                    :aria-describedby="`${id}-to-hint`"
                />
                <p :id="`${id}-to-hint`" class="hint" :class="{ invalid: !!toProblem }" role="status">{{ toSummary }}</p>

                <label :for="`${id}-subject`">Subject</label>
                <input :id="`${id}-subject`" v-model="subject" class="input" type="text" autocomplete="off" :aria-invalid="!!subjectProblem" />
                <p v-if="subjectProblem" class="hint invalid">{{ subjectProblem }}</p>

                <label :for="`${id}-text`">Text</label>
                <textarea :id="`${id}-text`" v-model="text" class="input body" rows="6" />

                <label class="acknowledge">
                    <input v-model="includeHtml" type="checkbox" />
                    <span>Add an HTML version</span>
                </label>
                <template v-if="includeHtml">
                    <label :for="`${id}-html`">HTML</label>
                    <textarea :id="`${id}-html`" v-model="html" class="input body" rows="8" spellcheck="false" />
                </template>

                <label class="acknowledge">
                    <input v-model="enhancedProtection" type="checkbox" />
                    <span>Enhanced Protection: send only over TLS, and hide the subject in the console</span>
                </label>

                <label class="acknowledge" data-testid="bypass-limits">
                    <input v-model="bypassLimits" type="checkbox" />
                    <span>
                        Skip sending limits
                        <span class="option-hint">
                            Not checked or counted against the sending rate, daily recipient quota or pending limit. Verified domain, DKIM, suppressions
                            and pause still apply.
                        </span>
                    </span>
                </label>

                <label v-if="recipientPolicy === 'allowlist'" class="acknowledge" data-testid="bypass-recipient-policy">
                    <input v-model="bypassRecipientPolicy" type="checkbox" />
                    <span>
                        Skip recipient allowlist
                        <span class="option-hint">Can deliver to people outside this deployment's recipient allowlist.</span>
                    </span>
                </label>
            </template>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="emit('close')">Cancel</button>
            <button type="submit" class="btn primary" :disabled="!canSubmit">
                {{ isSubmitting ? 'Sending…' : 'Send test message' }}
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { showConfirm, VfModal } from '@zyno-io/vue-foundation';
import { computed, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import type { TestMessageRequest } from '@/openapi-client-generated';
import { useModalPresence } from '@/shared/modal-state';
import {
    DEFAULT_TEST_SUBJECT,
    DEFAULT_TEST_TEXT,
    formatFrom,
    MAX_TEST_RECIPIENTS,
    parseAddresses,
    recipientProblem,
    validateDisplayName,
    validateLocalPart,
    validateSubject
} from '@/test-message';

import ApiErrorNotice from './api-error-notice.vue';
import Loader from './loader.vue';

const props = defineProps<{
    /** Active (verified) sending domains; null while loading. */
    domains: string[] | null;
    domainsError?: ApiErrorDescription | null;
    /** Domain to select first, when it is among `domains`. */
    preselect?: string | null;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
    /** The deployment's recipient policy, when the server reported it. The allowlist bypass is offered only for 'allowlist'. */
    recipientPolicy?: 'allowlist' | 'unrestricted' | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [request: TestMessageRequest];
}>();

useModalPresence();
const id = useId();
const { appId } = useAppContext();

// Form state lives only in this component; nothing is stored or put in the URL.
const local = ref('test');
const localTouched = ref(false);
const chosenDomain = ref<string | null>(null);
const displayName = ref('');
const toText = ref('');
const subject = ref(DEFAULT_TEST_SUBJECT);
const text = ref(DEFAULT_TEST_TEXT);
const includeHtml = ref(false);
const html = ref('');
const enhancedProtection = ref(false);
const bypassLimits = ref(false);
const bypassRecipientPolicy = ref(false);
/** True while the allowlist confirmation is open, so a second click cannot stack another. */
const confirming = ref(false);
/** The recipient bypass counts only while it is offered, so a stale tick cannot be sent. */
const bypassingRecipients = computed(() => props.recipientPolicy === 'allowlist' && bypassRecipientPolicy.value);

const domain = computed({
    get: () => {
        const list = props.domains ?? [];
        if (chosenDomain.value && list.includes(chosenDomain.value)) return chosenDomain.value;
        if (props.preselect && list.includes(props.preselect)) return props.preselect;
        return list[0] ?? '';
    },
    set: value => {
        chosenDomain.value = value;
    }
});

const parsed = computed(() => parseAddresses(toText.value));
const localProblem = computed(() => validateLocalPart(local.value));
const nameProblem = computed(() => validateDisplayName(displayName.value));
const subjectProblem = computed(() => validateSubject(subject.value));
const toProblem = computed(() => recipientProblem(parsed.value.valid.length, parsed.value.invalid));
const hasBody = computed(() => !!text.value.trim() || (includeHtml.value && !!html.value.trim()));
const toSummary = computed(() => {
    const { valid, invalid } = parsed.value;
    if (!valid.length && !invalid) return `Up to ${MAX_TEST_RECIPIENTS} recipients.`;
    return toProblem.value ?? `${valid.length} valid`;
});

const canSubmit = computed(
    () =>
        !props.isSubmitting &&
        !confirming.value &&
        !!props.domains?.length &&
        !!domain.value &&
        !localProblem.value &&
        !nameProblem.value &&
        !subjectProblem.value &&
        !toProblem.value &&
        parsed.value.valid.length > 0 &&
        hasBody.value
);

async function submit() {
    localTouched.value = true;
    if (!canSubmit.value) return;
    if (bypassingRecipients.value) {
        confirming.value = true;
        let confirmed = false;
        try {
            const count = parsed.value.valid.length;
            confirmed = await showConfirm(
                'Skip the recipient allowlist?',
                `This sends to ${count} ${count === 1 ? 'recipient' : 'recipients'} without checking this deployment's recipient allowlist, so the message can reach people outside it. The send is recorded in the audit log and the message is tagged as a console test.`
            );
        } finally {
            confirming.value = false;
        }
        if (!confirmed) return;
    }
    const request: TestMessageRequest = {
        from: formatFrom(displayName.value, local.value, domain.value),
        to: parsed.value.valid,
        subject: subject.value,
        ...(text.value.trim() ? { text: text.value } : {}),
        ...(includeHtml.value && html.value.trim() ? { html: html.value } : {}),
        ...(enhancedProtection.value ? { enhanced_protection: true } : {}),
        // Sent only when on: an absent flag is the safe default.
        ...(bypassLimits.value ? { bypass_limits: true } : {}),
        ...(bypassingRecipients.value ? { bypass_recipient_policy: true } : {})
    };
    emit('submit', request);
}
</script>

<style lang="scss" scoped>
.modal-body {
    gap: 8px;
    width: min(540px, calc(100vw - 72px));

    label:not(:has(input[type='checkbox'])) {
        color: var(--text);
        font-weight: 600;
    }

    .from-row {
        display: flex;
        align-items: center;
        gap: 6px;

        .input {
            min-width: 0;
        }

        .input:first-child {
            flex: 1;
        }

        .at {
            color: var(--text-3);
        }
    }

    .recipients,
    .body {
        resize: vertical;
        font-family: var(--font-mono, ui-monospace, monospace);
        font-size: 12.5px;
    }

    .hint.invalid {
        color: var(--danger);
    }
}

.acknowledge {
    align-items: flex-start;
    color: var(--text);

    .option-hint {
        display: block;
        color: var(--text-3);
        font-size: 12px;
    }
}
</style>
