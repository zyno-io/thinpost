<template>
    <section class="card" aria-labelledby="list-unsubscribe-heading" :aria-busy="isLoading || isSaving">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 id="list-unsubscribe-heading">
                    One-click unsubscribe
                    <template v-if="config">
                        <span v-if="config.enabled" class="tag success">On</span>
                        <span v-else class="tag">Off</span>
                    </template>
                </h2>
                <HelpButton topic="one-click unsubscribe">
                    <p>
                        When on, every message from this app includes a <code>List-Unsubscribe</code> header (RFC 8058) with a secure
                        one-click link. When a recipient's mail app, or the confirmation page the link opens, unsubscribes them, Thinpost adds the
                        address to this app's suppression list and sends one <code>unsubscribed</code> webhook event.
                    </p>
                    <ul>
                        <li>Off by default. Thinpost is built for transactional email, so turn this on only for an app that sends bulk or marketing mail. Gmail and Yahoo require one-click unsubscribe from bulk senders.</li>
                        <li>Needs tracking to be available for this Thinpost installation, and its tracking address to be reachable from the internet. Your Thinpost administrator sets this up.</li>
                        <li>While on, any send that includes its own <code>List-Unsubscribe</code> headers is rejected.</li>
                        <li>Thinpost skips unsubscribed addresses in later sends from this app. To send to one again, remove it from the Suppressions tab.</li>
                    </ul>
                </HelpButton>
            </div>
            <div class="actions">
                <button type="button" class="btn sm" :disabled="isLoading || isSaving || !isSignedIn" @click="load">
                    <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isLoading && !!config }" aria-hidden="true" />Refresh
                </button>
                <button
                    type="button"
                    class="btn sm"
                    :class="{ primary: config && !config.enabled }"
                    :disabled="!config || !!blocker || isLoading || isSaving"
                    data-testid="list-unsubscribe-toggle"
                    @click="toggle"
                >
                    <i class="fa-solid" :class="config?.enabled ? 'fa-ban' : 'fa-envelope-circle-check'" aria-hidden="true" />{{ config?.enabled ? 'Turn off' : 'Turn on' }}
                </button>
            </div>
        </div>
        <div class="card-body panel-body">
            <Loader v-if="isLoading && !config" label="Loading one-click unsubscribe" />
            <ApiErrorNotice v-if="loadError" :error="loadError" :retry="isSignedIn ? load : undefined" retry-label="Reload" />
            <p v-if="config && blocker" class="hint" data-testid="list-unsubscribe-blocker">{{ blocker }}</p>
            <ApiErrorNotice v-if="saveError" :error="saveError" />
            <p v-if="savedNote" class="saved" role="status"><i class="fa-solid fa-check" aria-hidden="true" /> {{ savedNote }}</p>
        </div>
    </section>
</template>

<script setup lang="ts">
import { showConfirm, showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, shallowRef } from 'vue';

import { apiErrorStatus, describeApiError, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { parseListUnsubscribe } from '@/list-unsubscribe';
import { DefaultApi, type ListUnsubscribe } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';

const props = defineProps<{
    /** Whether tracking is available; null when that is not known. */
    tracking: boolean | null;
}>();

const { app } = useAppContext();

const config = shallowRef<ListUnsubscribe | null>(null);
const isLoading = ref(false);
const isSaving = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const saveError = ref<ApiErrorDescription | null>(null);
const savedNote = ref<string | null>(null);

const isSignedIn = computed(() => authState.status === 'signed-in');
/** Turning it off is always allowed; turning it on needs tracking (the server returns 409 otherwise). */
const blocker = computed(() => {
    if (!isSignedIn.value) return 'Sign in again to change one-click unsubscribe.';
    if (config.value && !config.value.enabled && props.tracking === false) return 'Unavailable: tracking isn\'t available for this Thinpost installation. Ask your Thinpost administrator to enable it.';
    return null;
});

let seq = 0;
let isUnmounted = false;

async function load() {
    const mine = ++seq;
    isLoading.value = true;
    loadError.value = null;
    savedNote.value = null;
    try {
        const { data } = await DefaultApi.getListUnsubscribe({ path: { appId: app.value.id } });
        if (isUnmounted || mine !== seq) return;
        config.value = parseListUnsubscribe(data);
    } catch (err) {
        if (isUnmounted || mine !== seq) return;
        loadError.value = describeApiError(err, {
            notFound: 'The app was not found.',
            unavailable: 'One-click unsubscribe is temporarily unavailable. Try again in a moment.'
        });
    } finally {
        if (mine === seq) isLoading.value = false;
    }
}

async function toggle() {
    if (!config.value || blocker.value || isSaving.value) return;
    const target = app.value;
    const enable = !config.value.enabled;
    if (enable) {
        const ok = await showConfirm(
            'Turn on one-click unsubscribe?',
            `Every message sent by “${target.name}” will include a List-Unsubscribe header, and sends that include their own are rejected. Recipients who unsubscribe are added to the suppression list.`
        );
        if (!ok || isUnmounted) return;
    }
    isSaving.value = true;
    saveError.value = null;
    savedNote.value = null;
    seq++;
    isLoading.value = false;
    try {
        const { data } = await DefaultApi.setListUnsubscribe({ path: { appId: target.id }, body: { enabled: enable } });
        const saved = parseListUnsubscribe(data);
        if (isUnmounted) {
            showToast({ message: `One-click unsubscribe for “${target.name}” is now ${saved.enabled ? 'on' : 'off'} (saved after you left the page).`, durationSecs: 8 });
            return;
        }
        config.value = saved;
        savedNote.value = `Saved: ${saved.enabled ? 'on' : 'off'}.`;
        showToast({ message: `One-click unsubscribe ${saved.enabled ? 'turned on' : 'turned off'} for “${target.name}”.`, durationSecs: 5 });
    } catch (err) {
        if (isUnmounted) {
            showToast({ message: `The one-click unsubscribe change for “${target.name}” may not have completed. Open its Tracking tab to check it.`, durationSecs: 8 });
            return;
        }
        const status = apiErrorStatus(err);
        saveError.value = describeApiError(err, {
            mutation: true,
            notFound: 'The app was not found. Nothing was changed.',
            conflict:
                'One-click unsubscribe can\'t be turned on because tracking isn\'t available for this Thinpost installation, or its tracking address isn\'t usable. Ask your Thinpost administrator. Nothing was changed.',
            unavailable: 'Changing one-click unsubscribe is temporarily unavailable. Try again in a moment.'
        });
        // Without a definitive refusal the stored value may differ from the one shown.
        if (status === undefined || status >= 500) void load();
    } finally {
        isSaving.value = false;
    }
}

void load();

onBeforeUnmount(() => {
    isUnmounted = true;
});
</script>

<style lang="scss" scoped>
.card-header {
    flex-wrap: wrap;
}

.card-header h2 {
    display: flex;
    align-items: center;
    gap: 8px;
}

.actions {
    display: flex;
    gap: 8px;
}

.panel-body {
    display: flex;
    flex-direction: column;
    gap: 10px;
    font-size: 13px;
}

.saved {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--success);
}
</style>
