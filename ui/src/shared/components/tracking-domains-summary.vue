<template>
    <section class="card" aria-labelledby="tracking-domains-heading" :aria-busy="isLoading" data-testid="tracking-domains-summary">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 id="tracking-domains-heading">Your own tracking address</h2>
                <HelpButton topic="your own tracking address">
                    <p>
                        Each verified sending domain can have its own tracking domain, so links and the open-tracking image in your emails show your domain instead of Thinpost's. You set it
                        up on the Sending domains tab. This page only shows where each one stands.
                    </p>
                    <ul>
                        <li>New mail uses a tracking domain only once Thinpost can open it over a secure connection. Otherwise it uses Thinpost's standard tracking address.</li>
                        <li>One-click unsubscribe links always use Thinpost's standard address.</li>
                    </ul>
                </HelpButton>
            </div>
            <RouterLink v-if="available" class="btn sm" :to="{ name: 'app-domains', params: { appId } }">Manage on Sending domains</RouterLink>
        </div>
        <div class="card-body summary-body">
            <p v-if="!available" class="muted" data-testid="tracking-domains-unavailable">
                Using your own tracking address isn't turned on for this Thinpost installation. Links use Thinpost's standard tracking address. Ask your Thinpost administrator if you'd like it.
            </p>
            <template v-else>
                <Loader v-if="isLoading && !domains" label="Loading tracking domains" />
                <ApiErrorNotice v-if="loadError" :error="loadError" :retry="load" retry-label="Reload" />
                <p v-if="domains && !rows.length" class="muted">No sending domain has its own tracking address. Links use Thinpost's standard tracking address.</p>
                <ul v-if="rows.length" class="rows">
                    <li v-for="row in rows" :key="row.sender" :data-testid="`tracking-summary-${row.sender}`">
                        <span class="sender">{{ row.sender }}</span>
                        <template v-if="row.view">
                            <span class="mono name">{{ row.view.name }}</span>
                            <span class="tag" :class="row.tone">{{ row.label }}</span>
                        </template>
                        <span v-else class="muted">Standard tracking address</span>
                    </li>
                </ul>
            </template>
        </div>
    </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';

import { describeApiError, type ApiErrorDescription } from '@/api-errors';
import { DefaultApi, type Domain } from '@/openapi-client-generated';
import { parseTrackingDomain, trackingStatus, trackingSummaryLine } from '@/tracking-domains';

import ApiErrorNotice from './api-error-notice.vue';
import HelpButton from './help-button.vue';
import Loader from './loader.vue';

const props = defineProps<{
    appId: string;
    /** True when this installation lets apps use their own tracking domain (from the runtime report). */
    available: boolean;
}>();

const domains = shallowRef<Domain[] | null>(null);
const isLoading = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
let seq = 0;
let isUnmounted = false;
onBeforeUnmount(() => {
    isUnmounted = true;
});

const rows = computed(() =>
    (domains.value ?? [])
        .filter(d => d.active)
        .map(d => {
            const view = parseTrackingDomain(d.tracking_domain);
            const status = view ? trackingStatus(view, d.name, { available: true, cnameTarget: null }) : null;
            return { sender: d.name, view, label: view ? trackingSummaryLine(view) : '', tone: status?.tone ?? 'warning' };
        })
);

async function load() {
    const mine = ++seq;
    isLoading.value = true;
    loadError.value = null;
    try {
        const { data } = await DefaultApi.listAppDomains({ path: { appId: props.appId } });
        if (isUnmounted || mine !== seq) return;
        domains.value = Array.isArray(data?.domains) ? data.domains : [];
    } catch (err) {
        if (isUnmounted || mine !== seq) return;
        loadError.value = describeApiError(err, { notFound: 'The app was not found.', unavailable: "The tracking domains couldn't be loaded right now. Try again in a moment." });
    } finally {
        if (mine === seq) isLoading.value = false;
    }
}

watch(
    () => props.available,
    available => {
        if (available) void load();
    },
    { immediate: true }
);
</script>

<style lang="scss" scoped>
.summary-body {
    display: flex;
    flex-direction: column;
    gap: 10px;
    font-size: 13px;
}

.rows {
    display: grid;
    gap: 8px;
    margin: 0;
    padding: 0;
    list-style: none;

    li {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: 4px 12px;
    }
}

.sender {
    font-weight: 600;
}

.name {
    overflow-wrap: anywhere;
}
</style>
