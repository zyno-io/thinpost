<template>
    <nav aria-label="App sections" class="tabs app-tabs">
        <RouterLink
            v-for="tab in APP_SECTIONS"
            :key="tab.id"
            :to="{ name: tab.route, params: { appId } }"
            class="tab"
            :aria-current="tab.id === current ? (route.name === tab.route ? 'page' : 'true') : undefined"
        >
            <i :class="tab.icon" aria-hidden="true" />
            {{ tab.label }}
        </RouterLink>
    </nav>
</template>

<script setup lang="ts">
import { computed } from 'vue';
import { useRoute } from 'vue-router';

import { APP_SECTIONS } from '@/app-sections';

defineProps<{ appId: string }>();

const route = useRoute();
const current = computed(() => route.meta.section);
</script>

<style lang="scss" scoped>
.tab i {
    margin-right: 4px;
    font-size: 11px;
}
</style>
