<template>
    <div id="app-keys" class="keys-page">
        <AppStatusPanel />

        <SendingIpPanel />

        <SubmissionRatePanel />

        <section aria-labelledby="keys-heading" class="card keys">
            <div class="card-header">
                <h2 id="keys-heading">API keys</h2>
                <button type="button" class="btn primary" :disabled="!canIssue" :aria-describedby="app.active ? undefined : 'issue-hint'" @click="openIssue">
                    <i class="fa-solid fa-key" aria-hidden="true" />
                    Create key
                </button>
            </div>

            <div v-if="!app.active || keyActionError" class="card-body keys-body">
                <p v-if="!app.active" id="issue-hint" class="hint">This app is paused, so new API keys can't be created.</p>
                <ApiErrorNotice v-if="keyActionError" :error="keyActionError" />
            </div>

            <div v-if="isReloadingKeys && !keys" class="card-body">
                <Loader label="Loading API keys" />
            </div>
            <div v-else-if="keysError" class="card-body">
                <ApiErrorNotice :error="keysError" :retry="reloadKeys" />
            </div>
            <div v-else-if="keys && !keys.length" class="card-body empty">
                <i class="fa-solid fa-key" aria-hidden="true" />
                <h2>No API keys yet</h2>
            </div>

            <div v-else-if="keys" class="table-wrap" :aria-busy="isReloadingKeys">
                <table class="data">
                    <caption class="sr-only">API keys for {{ app.name }}</caption>
                    <thead>
                        <tr>
                            <th scope="col">Name</th>
                            <th scope="col">Status</th>
                            <th scope="col">Created</th>
                            <th scope="col">Key ID</th>
                            <th scope="col"><span class="sr-only">Actions</span></th>
                        </tr>
                    </thead>
                    <tbody>
                        <tr v-for="key in keys" :key="key.id" :class="{ inactive: !key.active }">
                            <th scope="row" class="name">{{ key.name }}</th>
                            <td>
                                <span class="tag" :class="{ success: key.active }">{{ key.active ? 'Active' : 'Revoked' }}</span>
                            </td>
                            <td>
                                <time :datetime="key.created_at">{{ formatDateTime(key.created_at) }}</time>
                            </td>
                            <td><code class="id">{{ key.id }}</code></td>
                            <td class="actions">
                                <button class="btn sm danger"
                                    v-if="key.active"
                                    type="button"
                                    :disabled="!!revokingId || authState.status !== 'signed-in'"
                                    @click="revoke(key)"
                                >
                                    {{ revokingId === key.id ? 'Revoking…' : 'Revoke' }}
                                    <span class="sr-only">{{ key.name }}</span>
                                </button>
                            </td>
                        </tr>
                    </tbody>
                </table>
            </div>
        </section>

        <NameModal
            v-if="isIssueOpen"
            title="Create API key"
            label="Key name"
            hint="A name to recognize this key by, such as the service that uses it. Up to 128 characters; accented and non-Latin characters count as more than one."
            submit-label="Create key"
            :is-submitting="isIssuing"
            :error="issueError"
            @close="closeIssue"
            @submit="issue"
        />

        <SecretModal
            v-if="issuedSecret"
            title="API key created"
            :subject="issuedSecret.keyName"
            :secret="issuedSecret.secret"
            @done="discardSecret"
        />
    </div>
</template>

<script lang="ts" setup>
import { showConfirmDestroy, showToast } from '@zyno-io/vue-foundation';
import { computed, ref, shallowRef, watch } from 'vue';

import { describeApiError, invalidResponse, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { formatDateTime } from '@/format';
import { DefaultApi, type Key } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import AppStatusPanel from '@/shared/components/app-status-panel.vue';
import Loader from '@/shared/components/loader.vue';
import NameModal from '@/shared/components/name-modal.vue';
import SecretModal from '@/shared/components/secret-modal.vue';
import SendingIpPanel from '@/shared/components/sending-ip-panel.vue';
import SubmissionRatePanel from '@/shared/components/submission-rate-panel.vue';
import { useOneTimeSecret } from '@/shared/one-time-secret';

const { app, appId } = useAppContext();

const keys = shallowRef<Key[] | null>(null);
const keysError = ref<ApiErrorDescription | null>(null);
const keyActionError = ref<ApiErrorDescription | null>(null);
const isReloadingKeys = ref(false);

const isIssueOpen = ref(false);
const isIssuing = ref(false);
const issueError = ref<ApiErrorDescription | null>(null);
const revokingId = ref<string | null>(null);

// The one-time secret lives only in this component's memory and is dropped on dismissal or unmount.
const {
    held: issuedSecret,
    isUnmounted,
    discard: discardSecret
} = useOneTimeSecret<{ keyName: string; secret: string }>({
    isPending: () => isIssuing.value,
    pendingPrompt: {
        title: 'Leave while a key is being created?',
        message: 'If the key is created, its secret won\'t be shown and you should revoke the key.'
    },
    heldPrompt: { title: 'Leave without saving the key?', message: 'The API key is still on screen and can\'t be shown again.' }
});

const canIssue = computed(() => app.value.active && authState.status === 'signed-in' && !isIssuing.value);

let keysSeq = 0;

watch(appId, () => void reloadKeys(), { immediate: true });

async function reloadKeys() {
    const id = appId.value;
    const reloadSeq = ++keysSeq;
    const isCurrent = () => reloadSeq === keysSeq;
    isReloadingKeys.value = true;
    keysError.value = null;
    try {
        const response = await DefaultApi.listAppKeys({ path: { appId: id } });
        const data = response.data;
        if (!isCurrent()) return;
        if (Array.isArray(data?.keys)) {
            keys.value = data.keys;
        } else {
            keys.value = null;
            keysError.value = invalidResponse();
        }
    } catch (err) {
        if (isCurrent()) {
            keys.value = null;
            keysError.value = describeApiError(err, { notFound: 'The app was not found.' });
        }
    } finally {
        if (reloadSeq === keysSeq) isReloadingKeys.value = false;
    }
}

function openIssue() {
    issueError.value = null;
    keyActionError.value = null;
    isIssueOpen.value = true;
}

function closeIssue() {
    if (isIssuing.value) return;
    isIssueOpen.value = false;
}

async function issue(name: string) {
    const id = appId.value;
    isIssuing.value = true;
    issueError.value = null;
    try {
        const response = await DefaultApi.issueAppKey({ path: { appId: id }, body: { name } });
        const data = response.data;
        if (isUnmounted()) {
            // The user confirmed leaving mid-request; the secret is dropped, never displayed later.
            showToast({ message: `Key “${name}” was created after you left, so its secret wasn't shown. Revoke it and create another.`, durationSecs: 10 });
            return;
        }
        isIssueOpen.value = false;
        if (data && typeof data.secret === 'string' && data.secret && typeof data.key?.name === 'string') {
            issuedSecret.value = { keyName: data.key.name, secret: data.secret };
        } else {
            keyActionError.value = {
                kind: 'failed',
                message: 'The key was created, but Thinpost didn\'t return its secret. Revoke the new key if it is listed, then create another.'
            };
        }
        void reloadKeys();
    } catch (err) {
        if (isUnmounted()) return;
        issueError.value = describeApiError(err, {
            mutation: true,
            notFound: 'The app was not found, or it is paused.'
        });
    } finally {
        isIssuing.value = false;
    }
}

async function revoke(key: Key) {
    const id = appId.value;
    if (revokingId.value) return;
    const ok = await showConfirmDestroy(
        'Revoke API key',
        `Revoke “${key.name}”? Anything still using this key will stop working. This can't be undone.`
    );
    if (!ok) return;

    revokingId.value = key.id;
    keyActionError.value = null;
    try {
        await DefaultApi.revokeAppKey({ path: { appId: id, keyId: key.id } });
        showToast({ message: `Revoked “${key.name}”`, durationSecs: 3 });
    } catch (err) {
        keyActionError.value = describeApiError(err, {
            mutation: true,
            notFound: 'The key wasn\'t found, or it was already revoked.'
        });
    } finally {
        revokingId.value = null;
    }
    await reloadKeys();
}
</script>

<style lang="scss" scoped>
.keys-page {
    display: flex;
    flex-direction: column;
    gap: 16px;
}

.keys-body {
    display: flex;
    flex-direction: column;
    gap: 12px;
}

.name {
    font-weight: 600;
    overflow-wrap: anywhere;
}

.id {
    color: var(--text-3);
    font-size: 11.5px;
    white-space: nowrap;
}

.actions {
    text-align: right;
    white-space: nowrap;
}
</style>
