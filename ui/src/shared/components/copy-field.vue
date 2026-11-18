<template>
    <div class="field copy-field">
        <label :for="id">{{ label }}</label>
        <div class="row">
            <input
                :id="id"
                ref="input"
                class="input mono"
                type="text"
                :value="value"
                readonly
                autocomplete="off"
                autocapitalize="off"
                spellcheck="false"
                @focus="select"
            />
            <button type="button" class="btn" @click="copy">
                <i class="fa-solid fa-copy" aria-hidden="true" />
                Copy
                <span class="sr-only">{{ label }}</span>
            </button>
        </div>
        <p class="copy-status" role="status">{{ status }}</p>
    </div>
</template>

<script setup lang="ts">
import { ref, useId } from 'vue';

import { copyText } from '@/format';

const props = defineProps<{ label: string; value: string }>();

const id = useId();
const input = ref<HTMLInputElement>();
const status = ref('');

function select() {
    input.value?.select();
}

async function copy() {
    const ok = await copyText(props.value);
    if (ok) {
        status.value = 'Copied to clipboard.';
    } else {
        status.value = 'Couldn\'t copy. Select the value and copy it yourself.';
        select();
    }
}
</script>

<style lang="scss" scoped>
.copy-field {
    min-width: 0;

    label {
        color: var(--text-3);
        font-size: 11px;
        letter-spacing: 0.5px;
        text-transform: uppercase;
    }
}

.row {
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
