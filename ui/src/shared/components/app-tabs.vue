<template>
    <nav aria-label="App sections" class="tabs app-tabs">
        <RouterLink
            v-for="tab in visibleTabs"
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
import { computed, onMounted } from 'vue';
import { useRoute } from 'vue-router';

import { APP_SECTIONS } from '@/app-sections';
import { loadRuntimeFeatures, runtimeFeatures } from '@/runtime-features';

defineProps<{ appId: string }>();

const route = useRoute();
const current = computed(() => route.meta.section);
// Tabs for optional features appear only once the server reports the feature.
const visibleTabs = computed(() => APP_SECTIONS.filter(tab => !tab.feature || runtimeFeatures.value?.[tab.feature] === true));

onMounted(() => void loadRuntimeFeatures());
</script>

<style lang="scss" scoped>
.tab i {
    margin-right: 4px;
    font-size: 11px;
}
</style>
