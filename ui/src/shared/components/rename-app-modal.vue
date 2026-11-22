<template>
    <NameModal
        title="Rename app"
        label="App name"
        hint="Up to 128 characters; accented and non-Latin characters count as more than one. API keys, domains and messages are not affected."
        submit-label="Rename app"
        :initial-name="currentName"
        :is-submitting="isSubmitting"
        :error="error"
        @close="emit('close')"
        @submit="name => emit('submit', name)"
    />
</template>

<script setup lang="ts">
import type { ApiErrorDescription } from '@/api-errors';

import NameModal from './name-modal.vue';

defineProps<{
    currentName: string;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [name: string];
}>();
</script>
