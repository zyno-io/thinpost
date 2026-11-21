<template>
    <NameModal
        title="Create app"
        label="App name"
        hint="A name to recognize this app by. Up to 128 characters; accented and non-Latin characters count as more than one."
        submit-label="Create app"
        :is-submitting="isSubmitting"
        :error="error"
        @close="emit('close')"
        @submit="name => emit('submit', { name, sendingIpId: choice || null })"
    >
        <template v-if="options.length">
            <label :for="`${id}-ip`">Sending IP</label>
            <select :id="`${id}-ip`" v-model="choice" class="input" :aria-describedby="`${id}-ip-hint`" data-testid="sending-ip-picker">
                <option value="">{{ MAIN_IP_LABEL }} (default)</option>
                <option v-for="ip in options" :key="ip.id" :value="ip.id">{{ ipLabel(ip) }}</option>
            </select>
            <p :id="`${id}-ip-hint`" class="hint">The address this app's mail is sent from. You can change it later.</p>
        </template>
    </NameModal>
</template>

<script setup lang="ts">
import { computed, ref, useId } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import type { SendingIp } from '@/openapi-client-generated';
import { ipLabel, MAIN_IP_LABEL } from '@/sending-ips';

import NameModal from './name-modal.vue';

const props = defineProps<{
    /** Sending IPs; the picker is hidden when there are none (or they couldn't be loaded). */
    ips: readonly SendingIp[];
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [input: { name: string; sendingIpId: string | null }];
}>();

const id = useId();
const choice = ref('');
// The main IP is the default choice, so its own row isn't listed twice. An IP the sending worker doesn't have can't be chosen.
const options = computed(() => props.ips.filter(ip => !ip.is_primary && ip.on_edge !== false));
</script>

<style lang="scss" scoped>
label {
    margin-top: 4px;
    color: var(--text);
    font-weight: 600;
}

.hint {
    color: var(--text-3);
    font-size: 12px;
}
</style>
