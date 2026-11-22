<template>
    <div class="app-shell">
        <AppHeader v-if="status !== 'not-found'" :app-id="routeAppId" :app="app" :apps="apps" :inert="isModalOpen" :can-rename="isReady" @rename="openRename" />

        <RenameAppModal
            v-if="renameTarget"
            :current-name="renameTarget.name"
            :is-submitting="isRenaming"
            :error="renameError"
            @close="closeRename"
            @submit="rename"
        />

        <div v-if="status === 'not-found'" class="card empty app-missing">
            <i class="fa-solid fa-cube" aria-hidden="true" />
            <h1>App not found</h1>
            <RouterLink to="/apps" class="btn">Back to apps</RouterLink>
        </div>

        <ApiErrorNotice v-else-if="status === 'error' && error" :error="error" :retry="retry" />

        <RouterView v-else-if="isReady" v-slot="{ Component }">
            <component :is="Component" :key="routeAppId" />
        </RouterView>

        <div v-else class="card shell-loading">
            <Loader label="Loading app" />
        </div>
    </div>
</template>

<script lang="ts" setup>
import { showToast } from '@zyno-io/vue-foundation';
import { computed, ref, shallowRef, watch } from 'vue';
import { useRoute } from 'vue-router';

import { describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { provideAppContext } from '@/app-context';
import { forgetApp, rememberApp } from '@/last-app';
import { DefaultApi, type App } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import AppHeader from '@/shared/components/app-header.vue';
import RenameAppModal from '@/shared/components/rename-app-modal.vue';
import Loader from '@/shared/components/loader.vue';
import { openModalCount } from '@/shared/modal-state';
import { authState } from '@/auth';
import { isOpaqueId } from '@/validation';

type ShellStatus = 'loading' | 'ready' | 'not-found' | 'error';

const route = useRoute();

const status = ref<ShellStatus>('loading');
const apps = shallowRef<App[] | null>(null);
/** The app to show in the header; may be a cached entry while loading. */
const app = shallowRef<App | null>(null);
/** Only ever assigned a loaded app. */
const readyApp = shallowRef<App | undefined>(undefined);
const loadedAt = ref(new Date());
const error = ref<ApiErrorDescription | null>(null);

let loadSeq = 0;

const routeAppId = computed(() => (typeof route.params.appId === 'string' ? route.params.appId : ''));
const isReady = computed(() => status.value === 'ready' && readyApp.value?.id === routeAppId.value);
// Keep keyboard focus out of the header while a modal (such as a one-time secret) is open.
const isModalOpen = computed(() => openModalCount.value > 0);

watch(
    routeAppId,
    id => {
        // The route has already left this shell while it unmounts; do not flash not-found.
        if (!('appId' in route.params)) return;
        void load(id);
    },
    { immediate: true }
);

function readApps(data: { apps?: unknown } | undefined): App[] {
    const list = data?.apps;
    if (!Array.isArray(list)) throw new InvalidResponseError();
    return list as App[];
}

async function load(id: string) {
    if (!isOpaqueId(id)) {
        loadSeq++;
        status.value = 'not-found';
        app.value = null;
        return;
    }
    const seq = ++loadSeq;
    app.value = apps.value?.find(candidate => candidate.id === id) ?? null;
    status.value = 'loading';
    error.value = null;
    try {
        const response = await DefaultApi.listApps();
        const list = readApps(response.data);
        if (seq !== loadSeq) return;
        apps.value = list;
        loadedAt.value = new Date();
        const found = list.find(candidate => candidate.id === id) ?? null;
        if (!found) {
            status.value = 'not-found';
            app.value = null;
            forgetApp(id);
            return;
        }
        readyApp.value = found;
        app.value = found;
        status.value = 'ready';
        rememberApp(id);
    } catch (err) {
        if (seq !== loadSeq) return;
        status.value = 'error';
        error.value = describeApiError(err);
    }
}

function retry() {
    void load(routeAppId.value);
}

async function reload(): Promise<App | null> {
    const id = readyApp.value?.id;
    if (!id) return null;
    const seq = loadSeq;
    const response = await DefaultApi.listApps();
    const list = readApps(response.data);
    const found = list.find(candidate => candidate.id === id) ?? null;
    if (seq === loadSeq) {
        apps.value = list;
        loadedAt.value = new Date();
        if (found) {
            readyApp.value = found;
            app.value = found;
        }
    }
    return found;
}

/** The app the open rename dialog was opened for; never re-read from the route. */
const renameTarget = shallowRef<App | null>(null);
/** Counts rename dialogs; a request only touches the dialog it was sent from. */
let renameDialog = 0;
/** The app whose rename request is in flight. */
const renamingId = ref<string | null>(null);
const isRenaming = computed(() => !!renameTarget.value && renamingId.value === renameTarget.value.id);
const renameError = ref<ApiErrorDescription | null>(null);

function openRename() {
    if (!isReady.value || !readyApp.value || authState.status !== 'signed-in') return;
    renameError.value = null;
    renameDialog++;
    renameTarget.value = readyApp.value;
}

function closeRename() {
    if (isRenaming.value) return;
    renameDialog++;
    renameTarget.value = null;
}

// Moving to another app closes the dialog, even mid-request: the request still
// applies to the app it was sent for and no longer touches a dialog.
watch(routeAppId, () => {
    renameDialog++;
    renameTarget.value = null;
    renameError.value = null;
});

async function rename(name: string) {
    const target = renameTarget.value;
    if (!target || renamingId.value === target.id) return;
    const dialog = renameDialog;
    const sameDialog = () => dialog === renameDialog && renameTarget.value === target;
    if (name === target.name) {
        closeRename();
        return;
    }
    renamingId.value = target.id;
    renameError.value = null;
    try {
        const { data } = await DefaultApi.renameApp({ path: { appId: target.id }, body: { name } });
        if (!data || data.id !== target.id || typeof data.name !== 'string') throw new InvalidResponseError();
        applyName(target.id, data.name);
        if (sameDialog()) {
            renameTarget.value = null;
            showToast({ message: `Renamed to “${data.name}”`, durationSecs: 3 });
        }
    } catch (err) {
        if (sameDialog()) renameError.value = describeApiError(err, { mutation: true, notFound: 'The app was not found. It may have been deleted.' });
    } finally {
        if (renamingId.value === target.id) renamingId.value = null;
    }
}

/** Applies a confirmed rename to the list and, if it is the shown app, to the header, keeping the rest of what is loaded. */
function applyName(id: string, name: string) {
    if (apps.value) apps.value = apps.value.map(candidate => (candidate.id === id ? { ...candidate, name } : candidate));
    if (readyApp.value?.id === id) {
        readyApp.value = { ...readyApp.value, name };
        app.value = readyApp.value;
    }
}

function update(next: App) {
    if (next.id !== readyApp.value?.id) return;
    readyApp.value = next;
    app.value = next;
    if (apps.value) apps.value = apps.value.map(candidate => (candidate.id === next.id ? next : candidate));
    loadedAt.value = new Date();
}

provideAppContext({
    appId: computed(() => readyApp.value?.id ?? ''),
    app: computed(() => readyApp.value as App),
    loadedAt,
    reload,
    update
});

// Never contains search terms or message content.
watch(
    [() => route.fullPath, () => app.value?.name],
    () => {
        const title = typeof route.meta.title === 'string' ? route.meta.title : null;
        if (!title || !app.value) return;
        document.title = `${title} · ${app.value.name} · Thinpost`;
    },
    { flush: 'post', immediate: true }
);
</script>

<style lang="scss" scoped>
.app-shell {
    display: flex;
    flex-direction: column;
    gap: 16px;
    min-width: 0;
}

.app-missing {
    margin-top: 28px;
}
</style>
