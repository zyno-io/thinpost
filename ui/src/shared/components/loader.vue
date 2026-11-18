<template>
    <div class="loader" role="status">
        <i class="fa-solid fa-circle-notch fa-spin" aria-hidden="true" />
        <span class="sr-only">{{ label }}</span>
    </div>
</template>

<script setup lang="ts">
withDefaults(defineProps<{ label?: string }>(), { label: 'Loading' });
</script>

<style lang="scss" scoped>
.loader {
    display: flex;
    justify-content: center;
    padding: 32px;
    color: var(--accent);
    font-size: 22px;
}
</style>
