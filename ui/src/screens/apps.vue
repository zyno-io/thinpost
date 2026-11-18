<template>
    <div id="apps">
        <div class="header">
            <div class="heading-with-help">
                <h1>
                    Apps
                </h1>
                <HelpButton topic="apps">
                    <p>Each app has its own API keys, sending domains and messages. You can revoke one app's keys without affecting the others.</p>
                </HelpButton>
            </div>
            <button type="button" class="btn primary" :disabled="!canCreate" @click="openCreate">
                <i class="fa-solid fa-plus" aria-hidden="true" />
                Create app
            </button>
        </div>

        <Loader v-if="isLoading" label="Loading apps" />

        <ApiErrorNotice v-else-if="loadError" :error="loadError" :retry="load" />

        <div v-else-if="apps && !apps.length" class="card empty">
            <i class="fa-solid fa-cube" aria-hidden="true" />
            <h2>No apps</h2>
        </div>

        <div v-else-if="apps" class="table-wrap">
            <table class="data">
                <caption class="sr-only">Apps</caption>
                <thead>
                    <tr>
                        <th scope="col">Name</th>
                        <th scope="col">Status</th>
                        <th scope="col">Sending IP</th>
                        <th scope="col">Created</th>
                        <th scope="col">ID</th>
                    </tr>
                </thead>
                <tbody>
                    <tr v-for="app in apps" :key="app.id" :class="{ inactive: !app.active }">
                        <th scope="row" class="name">
                            <RouterLink :to="{ name: 'app-keys', params: { appId: app.id } }">{{ app.name }}</RouterLink>
                        </th>
                        <td>
                            <span class="tag" :class="{ success: app.active }">{{ app.active ? 'Active' : 'Paused' }}</span>
                        </td>
                        <td class="nowrap" data-testid="sending-ip-cell">
                            <template v-if="app.sending_ip_id === null">{{ MAIN_IP_LABEL }}</template>
                            <template v-else-if="ipNames">{{ ipNames.get(app.sending_ip_id) ?? 'Unknown' }}</template>
                            <span v-else class="muted" title="The list of sending IPs couldn't be loaded.">Unknown</span>
                        </td>
                        <td>
                            <time :datetime="app.created_at">{{ formatDateTime(app.created_at) }}</time>
                        </td>
                        <td><code class="id">{{ app.id }}</code></td>
                    </tr>
                </tbody>
            </table>
        </div>

        <CreateAppModal
            v-if="isCreateOpen"
            :ips="ips"
            :is-submitting="isCreating"
            :error="createError"
            @close="closeCreate"
            @submit="create"
        />
    </div>
</template>

<script lang="ts" setup>
import { showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, onMounted, ref, shallowRef } from 'vue';
import { useRouter } from 'vue-router';

import { describeApiError, invalidResponse, type ApiErrorDescription } from '@/api-errors';
import { authState } from '@/auth';
import { formatDateTime } from '@/format';
import { DefaultApi, type App, type SendingIp } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import CreateAppModal from '@/shared/components/create-app-modal.vue';
import { MAIN_IP_LABEL, parseSendingIps } from '@/sending-ips';
import { isOpaqueId } from '@/validation';

const router = useRouter();

const apps = shallowRef<App[] | null>(null);
const isLoading = ref(true);
const loadError = ref<ApiErrorDescription | null>(null);

// The named sending IPs. A failure to load them never blocks the list: the
// column then says "Unknown" and the create form offers no IP choice.
const ips = shallowRef<SendingIp[]>([]);
const ipsLoaded = ref(false);
const ipNames = computed(() => (ipsLoaded.value ? new Map(ips.value.map(ip => [ip.id, ip.name])) : null));

const isCreateOpen = ref(false);
const isCreating = ref(false);
const createError = ref<ApiErrorDescription | null>(null);

// Creating requires a working, authorized session; listing establishes that.
const canCreate = computed(() => authState.status === 'signed-in' && !isLoading.value && !loadError.value);

let isUnmounted = false;
onMounted(load);
onBeforeUnmount(() => {
    isUnmounted = true;
});

async function loadIps() {
    try {
        const { data } = await DefaultApi.listSendingIps();
        if (isUnmounted) return;
        ips.value = parseSendingIps(data).ips;
        ipsLoaded.value = true;
    } catch {
        if (isUnmounted) return;
        ips.value = [];
        ipsLoaded.value = false;
    }
}

async function load() {
    isLoading.value = true;
    loadError.value = null;
    void loadIps();
    try {
        const { data } = await DefaultApi.listApps();
        if (Array.isArray(data?.apps)) {
            apps.value = data.apps;
        } else {
            apps.value = null;
            loadError.value = invalidResponse();
        }
    } catch (err) {
        apps.value = null;
        loadError.value = describeApiError(err);
    } finally {
        isLoading.value = false;
    }
}

function openCreate() {
    createError.value = null;
    isCreateOpen.value = true;
}

function closeCreate() {
    if (isCreating.value) return;
    isCreateOpen.value = false;
}

async function create({ name, sendingIpId }: { name: string; sendingIpId: string | null }) {
    isCreating.value = true;
    createError.value = null;
    try {
        const { data } = await DefaultApi.createApp({ body: sendingIpId ? { name, sending_ip_id: sendingIpId } : { name } });
        if (!data || !isOpaqueId(data.id) || typeof data.name !== 'string') {
            createError.value = invalidResponse({ mutation: true });
            return;
        }
        showToast({ message: `Created “${data.name}”`, durationSecs: 3 });
        if (isUnmounted) return;
        isCreateOpen.value = false;
        await router.push({ name: 'app-keys', params: { appId: data.id } });
    } catch (err) {
        if (isUnmounted) return;
        createError.value = describeApiError(err, { mutation: true, notFound: 'That sending IP was not found. It may have been removed. Choose another and try again.' });
    } finally {
        isCreating.value = false;
    }
}
</script>

<style lang="scss" scoped>
.intro {
    margin-bottom: 16px;
}

.name {
    font-weight: 600;
    overflow-wrap: anywhere;
}

.nowrap {
    white-space: nowrap;
}

.id {
    color: var(--text-3);
    font-size: 11.5px;
    white-space: nowrap;
}
</style>
