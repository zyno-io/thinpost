<template>
    <DnsRow label="Record">
        <p class="record mono" data-testid="cname-record">
            <span class="kind">CNAME</span> <span class="name">{{ record.name }}</span
            ><template v-if="record.target || record.observed"> → </template
            ><span v-if="record.observed" class="part" :class="record.observed.state" :data-state="record.observed.state" :title="record.observed.hint"
                >{{ record.observed.text }}<span class="sr-only"> ({{ record.observed.hint }})</span></span
            ><template v-else-if="record.target">{{ record.target }}</template>
        </p>
        <div v-if="record.fix" class="fix" data-testid="cname-fix">
            <span class="fix-label">{{ record.fix.kind === 'add' ? 'Add: ' : 'Update: ' }}</span>
            <code ref="fixValue" class="fix-value" data-testid="cname-fix-value">CNAME {{ record.fix.host }} {{ record.fix.value }}</code>
            <button type="button" class="btn sm" data-testid="cname-fix-copy-name" @click="copy('host')">
                <i class="fa-solid fa-copy" aria-hidden="true" />
                Copy name<span class="sr-only"> of the CNAME record</span>
            </button>
            <button type="button" class="btn sm" data-testid="cname-fix-copy-target" @click="copy('value')">
                <i class="fa-solid fa-copy" aria-hidden="true" />
                Copy target<span class="sr-only"> of the CNAME record</span>
            </button>
            <span class="sr-only" role="status" aria-live="polite" data-testid="cname-copy-status">{{ status }}</span>
        </div>
        <p v-if="note" class="muted" data-testid="cname-note">{{ note }}</p>
        <p v-if="checkedAt" class="muted" data-testid="cname-checked">
            Checked <time :datetime="checkedAt">{{ formatDateTime(checkedAt) }}</time>
        </p>
    </DnsRow>
</template>

<script setup lang="ts">
import { showToast } from '@zyno-io/vue-foundation';
import { nextTick, ref } from 'vue';

import type { CnameRecordView } from '@/bounce-domains';
import { copyText, formatDateTime, selectElementText } from '@/format';

import DnsRow from './dns-row.vue';

const props = defineProps<{
    record: CnameRecordView;
    note?: string | null;
    checkedAt?: string | null;
}>();

const fixValue = ref<HTMLElement>();
const status = ref('');

async function copy(part: 'host' | 'value') {
    const fix = props.record.fix;
    if (!fix) return;
    const ok = await copyText(fix[part]);
    showToast({ message: ok ? `Copied CNAME ${part === 'host' ? 'name' : 'target'}` : "Couldn't copy", durationSecs: 2 });
    // Cleared first so a repeat of the same result is announced again.
    status.value = '';
    await nextTick();
    status.value = ok ? 'Copied' : "Couldn't copy";
    if (!ok) selectElementText(fixValue.value);
}
</script>

<style lang="scss" scoped>
.record,
.fix code {
    font-size: 12px;
    line-height: 1.8;
    overflow-wrap: anywhere;
}

.kind {
    color: var(--text-3);
}

.part {
    border-radius: 3px;

    &.problem {
        padding: 1px 3px;
        background: var(--danger-soft);
        text-decoration: underline solid var(--danger);
        text-decoration-thickness: 2px;
        text-underline-offset: 3px;
    }

    &.missing {
        padding: 0 3px;
        outline: 1px dashed var(--success);
        outline-offset: 1px;
        color: var(--success);

        &::before {
            content: '+';
            margin-right: 1px;
            font-weight: 600;
        }
    }
}

.fix {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px 8px;
    font-size: 12px;
}

.fix-label {
    flex: none;
    min-width: 52px;
    color: var(--text-2);
    font-weight: 600;
}

.fix code {
    min-width: 0;
    color: var(--success);
}

.muted {
    color: var(--text-3);
    font-size: 12px;
}
</style>
