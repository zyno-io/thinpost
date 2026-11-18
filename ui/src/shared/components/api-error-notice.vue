<template>
    <div class="notice danger api-error" role="alert">
        <i class="fa-solid fa-circle-exclamation" aria-hidden="true" />
        <div class="notice-body">
            <p>{{ error.message }}</p>
        </div>
        <div v-if="retry" class="notice-actions">
            <button type="button" class="btn sm" @click="retry">{{ retryLabel ?? 'Try again' }}</button>
        </div>
    </div>
</template>

<script setup lang="ts">
import type { ApiErrorDescription } from '@/api-errors';

defineProps<{
    error: ApiErrorDescription;
    retry?: () => void;
    /** Label for the retry button; defaults to “Try again”. */
    retryLabel?: string;
}>();
</script>
