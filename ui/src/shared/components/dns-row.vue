<template>
    <section class="rec">
        <h4 class="label">{{ label }}</h4>
        <div class="body"><slot /></div>
    </section>
</template>

<script setup lang="ts">
defineProps<{ label: string }>();
</script>

<style lang="scss" scoped>
.rec {
    display: grid;
    grid-template-columns: 56px minmax(0, 1fr);
    gap: 4px 12px;
    padding: 10px 0;
    border-top: 1px solid var(--border);

    &:last-child {
        padding-bottom: 0;
    }

    @media (max-width: 600px) {
        grid-template-columns: minmax(0, 1fr);
    }
}

.label {
    color: var(--text-3);
    font-size: 11px;
    font-weight: 600;
    letter-spacing: 0.5px;
    line-height: 19px;
    text-transform: uppercase;
}

.body {
    display: flex;
    flex-direction: column;
    gap: 6px;
    min-width: 0;
}
</style>
