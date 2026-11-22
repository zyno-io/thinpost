<template>
    <header class="app-header">
        <nav class="crumbs" aria-label="Breadcrumb">
            <ol>
                <li><RouterLink to="/apps">Apps</RouterLink></li>
                <li class="crumb-switch">
                    <AppSwitcher :apps="apps" :current-id="appId" :current-name="app?.name ?? null" />
                </li>
                <li v-if="section">
                    <RouterLink v-if="isDetail" :to="{ name: isInboundDetail ? 'app-inbound' : 'app-messages', params: { appId } }">{{ section.label }}</RouterLink>
                    <span v-else aria-current="page">{{ section.label }}</span>
                </li>
                <li v-if="isDetail && messageId">
                    <span aria-current="page">{{ isInboundDetail ? 'Inbound message' : 'Message' }} {{ shortId(messageId) }}</span>
                </li>
            </ol>
        </nav>

        <div class="title-row">
            <h1 v-if="app">{{ app.name }}</h1>
            <h1 v-else><span class="skeleton name-skeleton" aria-hidden="true" /><span class="sr-only">Loading app</span></h1>
            <button v-if="app && canRename" type="button" class="btn ghost sm icon rename" data-testid="rename-app" @click="emit('rename')">
                <i class="fa-solid fa-pen" aria-hidden="true" />
                <span class="sr-only">Rename app</span>
            </button>
            <span v-if="app" class="tag app-status" :class="{ success: app.active }">{{ app.active ? 'Active' : 'Paused' }}</span>
            <div v-if="app" class="app-meta">
                <code class="app-id">{{ app.id }}</code>
                <button type="button" class="btn ghost sm icon" @click="copyId(app.id)">
                    <i class="fa-solid fa-copy" aria-hidden="true" />
                    <span class="sr-only">Copy app ID</span>
                </button>
                <span>
                    Created <time :datetime="app.created_at">{{ formatDateTime(app.created_at) }}</time>
                </span>
            </div>
        </div>

        <AppTabs :app-id="appId" />
    </header>
</template>

<script lang="ts" setup>
import { showToast } from '@zyno-io/vue-foundation';
import { computed } from 'vue';
import { useRoute } from 'vue-router';

import { sectionInfo } from '@/app-sections';
import { copyText, formatDateTime, shortId } from '@/format';
import type { App } from '@/openapi-client-generated';

import AppSwitcher from './app-switcher.vue';
import AppTabs from './app-tabs.vue';

const emit = defineEmits<{ rename: [] }>();

defineProps<{
    appId: string;
    /** Null until the app is known (not even cached). */
    app: App | null;
    apps: readonly App[] | null;
    /** Only a loaded app that matches the route can be renamed. */
    canRename?: boolean;
}>();

const route = useRoute();
const section = computed(() => sectionInfo(route.meta.section));
const isInboundDetail = computed(() => route.name === 'app-inbound-message');
const isDetail = computed(() => route.name === 'app-message' || isInboundDetail.value);
const messageId = computed(() => {
    const id = isInboundDetail.value ? route.params.inboundId : route.params.messageId;
    return typeof id === 'string' ? id : '';
});

async function copyId(id: string) {
    const ok = await copyText(id);
    showToast({ message: ok ? 'Copied app ID' : 'Couldn\'t copy', durationSecs: 2 });
}
</script>

<style lang="scss" scoped>
.app-header {
    position: sticky;
    top: var(--header-h);
    z-index: 20;
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 12px 0 0;
    background: var(--bg);
}

.crumbs ol {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 4px;
    margin: 0;
    padding: 0;
    color: var(--text-3);
    font-size: 12.5px;
    list-style: none;

    li + li::before {
        margin-right: 4px;
        content: '/';
    }

    li {
        display: flex;
        align-items: center;
        min-width: 0;
    }

    a {
        color: var(--text-3);
    }

    a:hover {
        color: var(--text);
    }

    [aria-current='page'] {
        color: var(--text-2);
    }
}

.title-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 10px 12px;

    h1 {
        font-size: 19px;
        font-weight: 650;
        letter-spacing: -0.01em;
        overflow-wrap: anywhere;
    }
}

.name-skeleton {
    width: 160px;
    height: 18px;
}

.app-meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px 8px;
    margin-left: auto;
    color: var(--text-3);
    font-size: 12px;

    .app-id {
        font-size: 11.5px;
        overflow-wrap: anywhere;
    }
}

@media (max-width: 640px) {
    .app-header {
        position: static;
    }

    .app-meta {
        margin-left: 0;
    }
}
</style>
