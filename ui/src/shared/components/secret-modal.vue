<template>
    <VfModal class="tp-modal" role="alertdialog" aria-modal="true" :aria-labelledby="`${id}-title`" :aria-describedby="`${id}-warning`" @form-submit="done">
        <template #header>
            <span :id="`${id}-title`">{{ title }}</span>
        </template>

        <div class="modal-body secret-body">
            <div :id="`${id}-warning`" class="notice warning">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <p class="notice-body">Shown once. Copy the {{ (secretLabel ?? 'secret').toLowerCase() }} for <strong>{{ subject }}</strong> now; you won't be able to see it again.</p>
            </div>

            <template v-if="username">
                <label :for="`${id}-username`">Username</label>
                <div class="secret-row">
                    <input
                        :id="`${id}-username`"
                        class="input mono"
                        type="text"
                        :value="username"
                        readonly
                        autocomplete="off"
                        autocapitalize="off"
                        spellcheck="false"
                        data-1p-ignore
                        data-lpignore="true"
                        @focus="selectField"
                    />
                    <button type="button" class="btn" @click="copyUsername">
                        <i class="fa-solid fa-copy" aria-hidden="true" />
                        Copy
                        <span class="sr-only">username</span>
                    </button>
                </div>
            </template>

            <label :for="`${id}-secret`">{{ secretLabel ?? 'Secret' }}</label>
            <div class="secret-row">
                <input
                    :id="`${id}-secret`"
                    ref="secretInput"
                    v-autofocus
                    class="input mono"
                    type="text"
                    :value="secret"
                    readonly
                    autocomplete="off"
                    autocapitalize="off"
                    spellcheck="false"
                    data-1p-ignore
                    data-lpignore="true"
                    @focus="selectSecret"
                />
                <button type="button" class="btn" @click="copy">
                    <i class="fa-solid fa-copy" aria-hidden="true" />
                    Copy
                </button>
            </div>
            <p class="copy-status" role="status">{{ copyStatus }}</p>
            <p v-if="$slots.default" class="hint"><slot /></p>
        </div>

        <template #footer>
            <button type="submit" class="btn primary">Done</button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { showConfirm, VfModal } from '@zyno-io/vue-foundation';
import { ref, useId } from 'vue';

import { copyText } from '@/format';
import { useModalPresence } from '@/shared/modal-state';

const props = defineProps<{
    /** Dialog title, for example “API key issued”. */
    title: string;
    /** What the secret belongs to, shown in bold in the warning. */
    subject: string;
    secret: string;
    /** Shown above the secret with its own Copy button, for secrets that come with a login name (SMTP credentials). */
    username?: string;
    /** What to call the secret; defaults to “Secret”. */
    secretLabel?: string;
}>();

const emit = defineEmits<{ done: [] }>();

useModalPresence();
const id = useId();
const secretInput = ref<HTMLInputElement>();
const copied = ref(false);
const copyStatus = ref('');

function selectSecret() {
    secretInput.value?.select();
}

function selectField(event: FocusEvent) {
    (event.target as HTMLInputElement).select();
}

async function copyUsername() {
    const ok = await copyText(props.username ?? '');
    copyStatus.value = ok ? 'Username copied to clipboard.' : 'Couldn\'t copy. Select the username and copy it yourself.';
}

async function copy() {
    const ok = await copyText(props.secret);
    if (ok) {
        copied.value = true;
        copyStatus.value = 'Copied to clipboard.';
    } else {
        copyStatus.value = 'Couldn\'t copy. Select the secret and copy it yourself.';
        selectSecret();
    }
}

async function done() {
    if (!copied.value) {
        const ok = await showConfirm('Close without copying?', 'You haven\'t used the Copy button, and the secret can\'t be shown again.');
        if (!ok) return;
    }
    emit('done');
}
</script>

<style lang="scss" scoped>
.secret-body {
    width: min(560px, calc(100vw - 72px));

    label {
        margin-top: 4px;
        color: var(--text);
        font-weight: 600;
    }
}

.secret-row {
    display: flex;
    gap: 8px;

    .input {
        flex: 1;
        min-width: 0;
        font-size: 12px;
    }
}

.copy-status {
    min-height: 16px;
    color: var(--text-3);
    font-size: 12px;
}
</style>
