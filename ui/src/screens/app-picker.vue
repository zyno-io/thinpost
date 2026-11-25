<template>
    <div class="app-picker">
        <div class="header">
            <div class="heading-with-help">
                <h1>
                    {{ title }}
                </h1>
                <HelpButton :topic="title.toLowerCase()">
                    <p>{{ intro }}</p>
                </HelpButton>
            </div>
        </div>

        <Loader v-if="isLoading" label="Loading apps" />

        <ApiErrorNotice v-else-if="loadError" :error="loadError" :retry="load" />

        <div v-else-if="apps && !apps.length" class="card empty">
            <i class="fa-solid fa-cube" aria-hidden="true" />
            <h2>No apps</h2>
            <RouterLink to="/apps">Go to apps</RouterLink>
        </div>

        <div v-else-if="apps" class="card admin-list" role="list" :aria-label="`Apps for ${title.toLowerCase()}`">
            <RouterLink v-for="app in apps" :key="app.id" :to="{ name: target, params: { appId: app.id } }" class="item" role="listitem">
                <span class="item-info">
                    <span class="name">{{ app.name }}</span>
                    <span v-if="!app.active" class="tag">Paused</span>
                </span>
                <i class="fa-solid fa-chevron-right chevron" aria-hidden="true" />
            </RouterLink>
        </div>
    </div>
</template>

<script lang="ts" setup>
import { onMounted, ref, shallowRef } from 'vue';

import { describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { sortAppsByName } from '@/app-order';
import { DefaultApi, type App } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';

defineProps<{
    title: string;
    intro: string;
    target: 'app-domains' | 'app-messages' | 'app-webhooks' | 'app-tracking';
}>();

const apps = shallowRef<App[] | null>(null);
const isLoading = ref(true);
const loadError = ref<ApiErrorDescription | null>(null);

onMounted(load);

async function load() {
    isLoading.value = true;
    loadError.value = null;
    try {
        const { data } = await DefaultApi.listApps();
        if (!Array.isArray(data?.apps)) throw new InvalidResponseError();
        apps.value = sortAppsByName(data.apps);
    } catch (err) {
        apps.value = null;
        loadError.value = describeApiError(err);
    } finally {
        isLoading.value = false;
    }
}
</script>

<style lang="scss" scoped>
.intro {
    margin-bottom: 16px;
}

.item {
    color: var(--text);
    text-decoration: none;

    &:hover {
        background: var(--surface-hover);
        text-decoration: none;
    }

    .name {
        overflow-wrap: anywhere;
    }
}

.chevron {
    color: var(--text-3);
    font-size: 11px;
}
</style>
