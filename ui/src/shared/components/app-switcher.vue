<template>
    <DropdownMenu :label="`Switch app${currentName ? ` (current: ${currentName})` : ''}`" align="left" trigger-class="crumb-switcher">
        <template #trigger>
            <span v-if="currentName" class="switcher-name">{{ currentName }}</span>
            <span v-else class="skeleton switcher-skeleton" aria-hidden="true" />
            <i class="fa-solid fa-chevron-down" aria-hidden="true" />
        </template>
        <div class="switcher-menu">
            <div class="menu-label">Apps</div>
            <div v-if="!apps" class="menu-meta" role="status">Loading apps</div>
            <RouterLink
                v-for="candidate in apps ?? []"
                :key="candidate.id"
                :to="{ name: targetRoute, params: { appId: candidate.id } }"
                class="menu-item"
                role="menuitemradio"
                :aria-checked="candidate.id === currentId"
            >
                <span class="switcher-item-name">{{ candidate.name }}</span>
                <span v-if="!candidate.active" class="tag">Paused</span>
                <i v-if="candidate.id === currentId" class="fa-solid fa-check check" aria-hidden="true" />
            </RouterLink>
            <div class="menu-separator" role="separator" />
            <RouterLink to="/apps" class="menu-item" role="menuitem">All apps</RouterLink>
        </div>
    </DropdownMenu>
</template>

<script lang="ts" setup>
import { computed } from 'vue';
import { useRoute } from 'vue-router';

import { sectionInfo } from '@/app-sections';
import type { App } from '@/openapi-client-generated';

import DropdownMenu from './dropdown-menu.vue';

defineProps<{
    apps: readonly App[] | null;
    currentId: string;
    currentName: string | null;
}>();

const route = useRoute();

// Switching keeps the current section; the message detail maps to its list. Never carries query or state.
const targetRoute = computed(() => sectionInfo(route.meta.section)?.route ?? 'app-keys');
</script>

<style lang="scss" scoped>
.switcher-menu {
    max-height: 60vh;
    overflow: auto;
}

.switcher-name {
    max-width: 220px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
}

.switcher-skeleton {
    width: 96px;
}

.switcher-item-name {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
}

:deep(.dropdown-trigger.crumb-switcher) {
    height: 24px;
    padding: 0 6px;
    color: var(--text-2);
    font-size: 12.5px;
    font-weight: 500;

    i {
        font-size: 9px;
    }
}
</style>
