<template>
    <DnsRow label="Record">
        <p class="record mono" data-testid="dkim-record-name">
            <span class="kind">TXT</span> <span ref="nameEl" class="name">{{ name }}</span>
        </p>
        <code ref="valueEl" class="value mono" data-testid="dkim-record-value">{{ shown }}</code>
        <div class="actions">
            <button type="button" class="btn sm" data-testid="dkim-copy-name" @click="copy('name')">
                <i class="fa-solid fa-copy" aria-hidden="true" />
                Copy name<span class="sr-only"> of the DKIM record</span>
            </button>
            <button type="button" class="btn sm" data-testid="dkim-copy-value" @click="copy('value')">
                <i class="fa-solid fa-copy" aria-hidden="true" />
                Copy value<span class="sr-only"> of the DKIM record</span>
            </button>
            <label class="check split">
                <input v-model="splitValue" type="checkbox" data-testid="dkim-split" />
                <span>Split into 255-character strings</span>
            </label>
            <span class="sr-only" role="status" aria-live="polite" data-testid="dkim-copy-status">{{ status }}</span>
        </div>
        <p v-if="note" class="muted" data-testid="dkim-note">{{ note }}</p>
    </DnsRow>
</template>

<script setup lang="ts">
import { showToast } from '@zyno-io/vue-foundation';
import { computed, nextTick, ref } from 'vue';

import { splitTxtValue } from '@/dkim';
import { copyText, selectElementText } from '@/format';

import DnsRow from './dns-row.vue';

const props = defineProps<{ name: string; value: string; note?: string | null }>();

const splitValue = ref(false);
const shown = computed(() => (splitValue.value ? splitTxtValue(props.value) : props.value));
const nameEl = ref<HTMLElement>();
const valueEl = ref<HTMLElement>();
const status = ref('');

async function copy(part: 'name' | 'value') {
    const ok = await copyText(part === 'name' ? props.name : shown.value);
    showToast({ message: ok ? `Copied DKIM record ${part}` : "Couldn't copy", durationSecs: 2 });
    // Cleared first so a repeat of the same result is announced again.
    status.value = '';
    await nextTick();
    status.value = ok ? 'Copied' : "Couldn't copy";
    if (!ok) selectElementText(part === 'name' ? nameEl.value : valueEl.value);
}
</script>

<style lang="scss" scoped>
.record,
.value {
    font-size: 12px;
    overflow-wrap: anywhere;
}

.record {
    line-height: 1.8;
}

.kind {
    color: var(--text-3);
}

.value {
    display: block;
    padding: 6px 8px;
    border: 1px solid var(--border);
    border-radius: 6px;
    background: var(--surface-2);
    line-height: 1.5;
}

.actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px 8px;
}

.split {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    margin-left: 4px;
    font-size: 12px;
    font-weight: 400;
}

.muted {
    color: var(--text-3);
    font-size: 12px;
}
</style>
