<template>
    <div class="dkim-record">
        <CopyField label="Record name" :value="name" />
        <CopyField :label="splitValue ? 'Record value (split into 255-character strings)' : 'Record value'" :value="shown" />
        <label class="check split">
            <input v-model="splitValue" type="checkbox" />
            <span>
                Split into 255-character strings
                <span class="hint">Some DNS providers only take TXT values in pieces of 255 characters or fewer. Turn this on if yours refuses the value.</span>
            </span>
        </label>
    </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue';

import { splitTxtValue } from '@/dkim';

import CopyField from './copy-field.vue';

const props = defineProps<{ name: string; value: string }>();

const splitValue = ref(false);
const shown = computed(() => (splitValue.value ? splitTxtValue(props.value) : props.value));
</script>

<style lang="scss" scoped>
.dkim-record {
    display: grid;
    gap: 12px;
}

.split {
    display: flex;
    align-items: flex-start;
    gap: 8px;
    font-weight: 400;
}
</style>
