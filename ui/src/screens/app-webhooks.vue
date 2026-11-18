<template>
    <div id="app-webhooks">
        <Loader v-if="isLoading" label="Loading webhooks" />

        <ApiErrorNotice v-else-if="loadError" :error="loadError" :retry="load" />

        <template v-else>
            <section aria-labelledby="webhooks-heading" class="card">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="webhooks-heading">Webhooks</h2>
                        <HelpButton topic="webhooks">
                            <p>
                                Thinpost sends JSON events about your messages to these HTTPS addresses, in the format you pick when you add one: Thinpost,
                                Postmark, Resend or Mailgun. A signing secret is shown once, when you add a webhook or replace its secret. You can't see
                                it again. Postmark webhooks have no secret.
                            </p>
                            <p>
                                The Inbound mail format is different: it sends the emails your app receives at its receiving domains, not events about mail you
                                send. Add one before you point a receiving domain's MX record at Thinpost.
                            </p>
                            <p>
                                An event may arrive more than once, so ignore repeats using
                                <code>X-Thinpost-Event-ID</code> (Thinpost), <code>X-PM-Webhook-Trace-Id</code> (Postmark), <code>svix-id</code> (Resend) or
                                <code>event-data.id</code> (Mailgun). Only public internet addresses on port 443 can receive webhooks.
                            </p>
                            <p>
                                Disabling a webhook stops new events from being queued for it and pauses events already queued until you enable it again.
                                An event already on its way may still arrive. Failed events are retried automatically a limited number of times, and you can
                                then resend them from the webhook event history.
                            </p>
                            <p>
                                Postmark, Resend and Mailgun formats leave out details Thinpost doesn't record, such as the From address, tags, and client or
                                location details. Subjects of messages sent with Enhanced Protection are never included.
                            </p>
                            <p><strong>Not available yet:</strong> SendGrid-style webhooks.</p>
                        </HelpButton>
                    </div>
                    <button type="button" class="btn primary" :disabled="!canAdd" :aria-describedby="addHint ? 'add-hint' : undefined" @click="openCreate">
                        <i class="fa-solid fa-plus" aria-hidden="true" />
                        Add webhook
                    </button>
                </div>

                <div class="card-body webhooks-body">
                    <p v-if="addHint" id="add-hint" class="hint">{{ addHint }}</p>

                    <ApiErrorNotice v-if="actionError" :error="actionError" />

                    <div v-if="testResult" class="notice" :class="testResult.tone" role="status">
                        <i class="fa-solid" :class="testResult.tone === 'success' ? 'fa-circle-check' : 'fa-triangle-exclamation'" aria-hidden="true" />
                        <p class="notice-body">{{ testResult.message }}</p>
                    </div>

                    <Loader v-if="isReloading && !webhooks" label="Loading webhooks" />
                    <ApiErrorNotice v-else-if="listError" :error="listError" :retry="reloadWebhooks" />

                    <div v-else-if="webhooks && !webhooks.length" class="empty">
                        <i class="fa-solid fa-bolt" aria-hidden="true" />
                        <h2>No webhooks yet</h2>
                    </div>
                </div>

                <div v-if="webhooks && webhooks.length && !listError" class="table-wrap" :aria-busy="isReloading">
                    <table class="data">
                        <caption class="sr-only">
                            Webhooks for
                            {{
                                app.name
                            }}
                        </caption>
                        <thead>
                            <tr>
                                <th scope="col">URL</th>
                                <th scope="col">Format</th>
                                <th scope="col">Events</th>
                                <th scope="col">Status</th>
                                <th scope="col">Signing secret</th>
                                <th scope="col"><span class="sr-only">Actions</span></th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="hook in webhooks" :key="hook.id" :class="{ inactive: !hook.active }">
                                <th scope="row" class="endpoint-cell">
                                    <code class="endpoint">{{ hook.endpoint }}</code>
                                    <span v-if="hook.description" class="description">{{ hook.description }}</span>
                                    <span class="webhook-id"
                                        >ID <code>{{ hook.id }}</code></span
                                    >
                                    <span v-if="hook.auth" class="webhook-auth">{{ describeAuth(hook.auth) }}</span>
                                    <span v-else-if="hook.format === 'postmark'" class="webhook-warn">
                                        <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                                        Unsigned. Add Basic auth or a header so your receiver can trust it.
                                    </span>
                                </th>
                                <td>
                                    <span class="tag">{{ WEBHOOK_FORMATS[hook.format].tag }}</span>
                                </td>
                                <td>
                                    <span v-if="WEBHOOK_FORMATS[hook.format].receivesMail" class="events">Received mail</span>
                                    <span v-else-if="!hook.events" class="events">All events</span>
                                    <span v-else class="events" :title="hook.events.join(', ')"
                                        >{{ hook.events.length }} event {{ hook.events.length === 1 ? 'type' : 'types' }}</span
                                    >
                                </td>
                                <td>
                                    <span class="tag" :class="{ success: hook.active }">{{ hook.active ? 'Enabled' : 'Disabled' }}</span>
                                </td>
                                <td class="events">
                                    <template v-if="hook.secret_created_at">
                                        Created
                                        <time :datetime="hook.secret_created_at">{{ formatDateTime(hook.secret_created_at) }}</time>
                                    </template>
                                    <span v-else class="muted">Unsigned</span>
                                </td>
                                <td class="actions">
                                    <div class="action-buttons">
                                        <button class="btn sm" type="button" :disabled="!canAct" @click="sendTest(hook)">
                                            {{ isBusy(hook.id, 'test') ? 'Sending…' : 'Send test' }}
                                            <span class="sr-only">to {{ hook.endpoint }}</span>
                                        </button>
                                        <button
                                            class="btn sm"
                                            :class="hook.active ? 'danger' : 'primary'"
                                            type="button"
                                            :disabled="!canAct || (!hook.active && !app.active)"
                                            :aria-describedby="!hook.active && !app.active ? 'add-hint' : undefined"
                                            @click="hook.active ? disable(hook) : enable(hook)"
                                        >
                                            {{ isBusy(hook.id, 'toggle') ? (hook.active ? 'Disabling…' : 'Enabling…') : hook.active ? 'Disable' : 'Enable' }}
                                            <span class="sr-only">webhook to {{ hook.endpoint }}</span>
                                        </button>
                                        <DropdownMenu :label="`More actions for ${hook.endpoint}`" trigger-class="btn sm">
                                            <template #trigger>
                                                <i class="fa-solid fa-ellipsis" aria-hidden="true" />
                                            </template>
                                            <button
                                                class="menu-item"
                                                type="button"
                                                role="menuitem"
                                                :aria-disabled="!canAct ? 'true' : undefined"
                                                @click="openEdit(hook)"
                                            >
                                                <i class="fa-solid fa-pen" aria-hidden="true" />
                                                Edit
                                            </button>
                                            <button
                                                v-if="WEBHOOK_FORMATS[hook.format].secretKind"
                                                class="menu-item"
                                                type="button"
                                                role="menuitem"
                                                :aria-disabled="!canAct ? 'true' : undefined"
                                                @click="rotate(hook)"
                                            >
                                                <i class="fa-solid fa-key" aria-hidden="true" />
                                                Replace secret
                                            </button>
                                            <div class="menu-separator" role="separator" />
                                            <button
                                                class="menu-item"
                                                type="button"
                                                role="menuitem"
                                                :aria-disabled="!canAct ? 'true' : undefined"
                                                @click="remove(hook)"
                                            >
                                                <i class="fa-solid fa-trash" aria-hidden="true" />
                                                Delete
                                            </button>
                                        </DropdownMenu>
                                    </div>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </section>

            <WebhookDeliveryHistory
                :key="app.id"
                :app-id="app.id"
                :app-active="app.active"
                :subscriptions="endpointSummaries"
                :runtime-unavailable="listError?.status === 503"
                @changed="reloadWebhooks"
            />
        </template>

        <WebhookEndpointModal
            v-if="modal"
            :initial="modal.kind === 'edit' ? modal.initial : undefined"
            :preset-format="modal.kind === 'create' ? modal.preset : undefined"
            :is-submitting="isSubmitting"
            :error="modalError"
            @close="closeModal"
            @submit="submitModal"
        />

        <SecretModal v-if="issued" :title="issued.title" :subject="issued.subject" :secret="issued.secret" @done="discardSecret">
            {{ secretUsage(issued.format) }}
        </SecretModal>
    </div>
</template>

<script lang="ts" setup>
import { showConfirm, showConfirmDestroy, showToast } from '@zyno-io/vue-foundation';
import { computed, ref, shallowRef, watch } from 'vue';
import { useRoute, useRouter } from 'vue-router';

import { apiErrorStatus, describeApiError, InvalidResponseError, UNKNOWN_OUTCOME, type ApiErrorDescription, type DescribeOptions } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { formatDateTime } from '@/format';
import {
    DefaultApi,
    type Webhook,
    type WebhookAuthInputWritable,
    type WebhookEventType,
    type WebhookFormat,
    type WebhookTestResult
} from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import DropdownMenu from '@/shared/components/dropdown-menu.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import SecretModal from '@/shared/components/secret-modal.vue';
import WebhookDeliveryHistory, { type SubscriptionSummary } from '@/shared/components/webhook-delivery-history.vue';
import WebhookEndpointModal, { type WebhookEndpointSubmission, type WebhookEndpointValues } from '@/shared/components/webhook-endpoint-modal.vue';
import { useOneTimeSecret } from '@/shared/one-time-secret';
import { isOpaqueId } from '@/validation';
import { eventsOfFormat, WEBHOOK_FORMAT_ORDER, WEBHOOK_FORMATS } from '@/webhook-events';

type BusyAction = 'test' | 'toggle' | 'rotate' | 'delete';
type ModalState = { kind: 'create'; preset?: WebhookFormat } | { kind: 'edit'; hook: Webhook; initial: WebhookEndpointValues };

const UNAVAILABLE = 'Webhooks are unavailable right now. Try again in a moment. If it keeps happening, ask your Thinpost administrator.';

const { app, appId } = useAppContext();
const route = useRoute();
const router = useRouter();

const webhooks = shallowRef<Webhook[] | null>(null);
const isLoading = ref(true);
const loadError = ref<ApiErrorDescription | null>(null);
const listError = ref<ApiErrorDescription | null>(null);
const actionError = ref<ApiErrorDescription | null>(null);
const isReloading = ref(false);
const busy = ref<{ id: string; action: BusyAction } | null>(null);
const testResult = ref<{ tone: 'success' | 'warning'; message: string } | null>(null);

const modal = shallowRef<ModalState | null>(null);
const modalError = ref<ApiErrorDescription | null>(null);
const isSubmitting = ref(false);
// True only while a create or rotate (the requests that return a secret) is in flight.
const creating = ref(false);

// A signing secret is shown once, held only in this component's memory and dropped on dismissal or unmount.
const {
    held: issued,
    isUnmounted,
    discard: discardSecret
} = useOneTimeSecret<{
    title: string;
    subject: string;
    secret: string;
    format: WebhookFormat;
}>({
    isPending: () => creating.value || busy.value?.action === 'rotate',
    pendingPrompt: {
        title: 'Leave while a signing secret is being created?',
        message: 'If it is created, it won’t be shown. Replace the webhook’s secret afterwards to get a new one.'
    },
    heldPrompt: {
        title: 'Leave without saving the secret?',
        message: 'The signing secret is still on screen and can’t be shown again.'
    }
});

const isSignedIn = computed(() => authState.status === 'signed-in');
const hasMutation = computed(() => !!busy.value || isSubmitting.value);
const canAct = computed(() => isSignedIn.value && !hasMutation.value);
const listUnavailable = computed(() => !webhooks.value || listError.value?.status === 503);
const canAdd = computed(() => app.value.active && canAct.value && !listUnavailable.value);
const addHint = computed(() => {
    if (!app.value.active) return 'This app is paused, so webhooks can\'t be added or enabled.';
    if (listUnavailable.value) return 'The webhook list hasn\'t loaded.';
    return null;
});

/** Endpoint state for replay pre-checks; null while the list is not loaded. */
const endpointSummaries = computed<ReadonlyMap<string, SubscriptionSummary> | null>(() => {
    if (!webhooks.value) return null;
    return new Map(
        webhooks.value.map(hook => [
            hook.id,
            {
                endpoint: hook.endpoint,
                status: hook.active ? 'ready' : 'disabled'
            } as SubscriptionSummary
        ])
    );
});

let loadSeq = 0;
let reloadSeq = 0;

watch(appId, load, { immediate: true });

function isWebhook(value: unknown): value is Webhook {
    const item = value as Partial<Webhook> | null;
    if (!item || !isOpaqueId(item.id) || typeof item.format !== 'string' || !WEBHOOK_FORMAT_ORDER.includes(item.format)) return false;
    const known: readonly string[] = eventsOfFormat(item.format);
    const auth = item.auth as { username?: unknown; header_names?: unknown } | undefined;
    return (
        typeof item.endpoint === 'string' &&
        typeof item.description === 'string' &&
        typeof item.active === 'boolean' &&
        typeof item.created_at === 'string' &&
        (item.secret_created_at === undefined || typeof item.secret_created_at === 'string') &&
        (item.events === undefined || (Array.isArray(item.events) && item.events.every(e => typeof e === 'string' && known.includes(e)))) &&
        (auth === undefined ||
            (typeof auth === 'object' &&
                auth !== null &&
                (auth.username === undefined || typeof auth.username === 'string') &&
                (auth.header_names === undefined || (Array.isArray(auth.header_names) && auth.header_names.every(n => typeof n === 'string')))))
    );
}

/** Keeps only contract fields so nothing unexpected in a response is retained. */
function pick(hook: Webhook): Webhook {
    const { id, format, endpoint, description, events, active, created_at, secret_created_at, auth } = hook;
    const authNames =
        auth && (auth.username || auth.header_names?.length)
            ? {
                  ...(auth.username ? { username: auth.username } : {}),
                  ...(auth.header_names?.length ? { header_names: [...auth.header_names] } : {})
              }
            : undefined;
    return {
        id,
        format,
        endpoint,
        description,
        ...(events?.length ? { events: [...events] } : {}),
        active,
        created_at,
        ...(secret_created_at ? { secret_created_at } : {}),
        ...(authNames ? { auth: authNames } : {})
    };
}

function describeAuth(auth: NonNullable<Webhook['auth']>): string {
    const parts: string[] = [];
    if (auth.username) parts.push('Basic');
    const count = auth.header_names?.length ?? 0;
    if (count) parts.push(`${count} ${count === 1 ? 'header' : 'headers'}`);
    return parts.join(' · ');
}

function secretUsage(format: WebhookFormat): string {
    if (format === 'resend') return 'Use this as the Resend webhook signing secret (whsec_…) with resend.webhooks.verify or the Svix libraries.';
    if (format === 'mailgun') return 'Use this as the Mailgun HTTP webhook signing key.';
    return 'Use this exact text as the HMAC-SHA256 key to check signatures.';
}

async function fetchWebhooks(id: string): Promise<{ hooks: Webhook[]; error: null } | { hooks: null; error: unknown }> {
    try {
        const response = await DefaultApi.listAppWebhooks({ path: { appId: id } });
        const list: unknown = response?.data?.webhooks;
        if (!Array.isArray(list) || !list.every(isWebhook)) return { hooks: null, error: new InvalidResponseError() };
        return { hooks: list.map(pick), error: null };
    } catch (error) {
        return { hooks: null, error };
    }
}

function describeListError(err: unknown): ApiErrorDescription {
    return describeApiError(err, {
        notFound: 'The app was not found.',
        unavailable: UNAVAILABLE
    });
}

async function load() {
    const id = appId.value;
    const seq = ++loadSeq;
    isLoading.value = true;
    loadError.value = null;
    listError.value = null;
    actionError.value = null;
    testResult.value = null;
    modal.value = null;
    webhooks.value = null;

    const result = await fetchWebhooks(id);
    if (seq !== loadSeq) return;
    if (result.error === null) webhooks.value = result.hooks;
    else if (apiErrorStatus(result.error) === 404) loadError.value = describeListError(result.error);
    else listError.value = describeListError(result.error);
    isLoading.value = false;
    openRequestedCreate();
}

/**
 * The Inbound tab links here with `?add=thinpost_inbound` to open the add dialog with that format chosen. The query
 * carries no data, and is dropped so a reload or the back button does not open the dialog again.
 */
function openRequestedCreate() {
    if (route?.query.add !== 'thinpost_inbound') return;
    void router.replace({ name: route.name ?? undefined, params: route.params, query: {}, hash: route.hash });
    if (!canAdd.value) return;
    modalError.value = null;
    actionError.value = null;
    modal.value = { kind: 'create', preset: 'thinpost_inbound' };
}

async function reloadWebhooks() {
    const id = app.value?.id;
    if (!id) return;
    const seq = loadSeq;
    const thisReload = ++reloadSeq;
    isReloading.value = true;
    listError.value = null;
    try {
        const result = await fetchWebhooks(id);
        if (seq !== loadSeq || thisReload !== reloadSeq) return;
        if (result.error === null) {
            webhooks.value = result.hooks;
        } else {
            webhooks.value = null;
            listError.value = describeListError(result.error);
        }
    } finally {
        if (thisReload === reloadSeq) isReloading.value = false;
    }
}

function isBusy(id: string, action: BusyAction) {
    return busy.value?.id === id && busy.value.action === action;
}

const ERRORS: Record<BusyAction, DescribeOptions> = {
    test: {
        mutation: true,
        notFound: 'This webhook wasn\'t found. It may have been deleted.',
        unavailable: UNAVAILABLE
    },
    toggle: {
        mutation: true,
        notFound: 'The webhook wasn\'t found, or the app is paused.',
        conflict: '10 webhooks are already enabled. Disable one first.',
        unavailable: UNAVAILABLE
    },
    rotate: {
        mutation: true,
        conflict: 'Postmark webhooks have no signing secret to replace.',
        notFound: 'This webhook wasn\'t found. It may have been deleted.',
        unavailable: UNAVAILABLE
    },
    delete: {
        mutation: true,
        notFound: 'This webhook was already deleted.',
        unavailable: UNAVAILABLE
    }
};
const EDIT_ERRORS: DescribeOptions = {
    mutation: true,
    notFound: 'The webhook wasn\'t found. It may have been deleted.',
    unavailable: UNAVAILABLE
};

// ---- add and edit ----

function openCreate() {
    if (!canAdd.value) return;
    modalError.value = null;
    actionError.value = null;
    modal.value = { kind: 'create' };
}

function openEdit(hook: Webhook) {
    if (!canAct.value) return;
    modalError.value = null;
    actionError.value = null;
    modal.value = {
        kind: 'edit',
        hook,
        initial: {
            format: hook.format,
            endpoint: hook.endpoint,
            description: hook.description,
            events: [...(hook.events ?? [])],
            ...(hook.auth
                ? {
                      auth: {
                          ...(hook.auth.username ? { username: hook.auth.username } : {}),
                          headerNames: [...(hook.auth.header_names ?? [])]
                      }
                  }
                : {})
        }
    };
}

function closeModal() {
    if (isSubmitting.value) return;
    modal.value = null;
}

async function submitModal(values: WebhookEndpointSubmission) {
    const state = modal.value;
    if (!state || isSubmitting.value || !isSignedIn.value) return;
    if (state.kind === 'create') await create(values);
    else await update(state.hook, values);
}

function holdSecret(title: string, subject: string, format: WebhookFormat, secret: unknown): boolean {
    if (typeof secret !== 'string' || !secret) return false;
    issued.value = { title, subject, secret, format };
    return true;
}

async function create(values: WebhookEndpointSubmission) {
    const id = appId.value;
    const seq = loadSeq;
    isSubmitting.value = true;
    creating.value = true;
    modalError.value = null;
    try {
        const response = await DefaultApi.createAppWebhook({
            path: { appId: id },
            body: {
                format: values.format,
                endpoint: values.endpoint,
                description: values.description,
                ...(values.events.length ? { events: values.events } : {}),
                ...(values.auth ? { auth: values.auth } : {})
            }
        });
        const data = response?.data;
        if (isUnmounted() || seq !== loadSeq) {
            // The operator confirmed leaving mid-request; the secret is dropped, never displayed later.
            if (!isUnmounted()) modal.value = null;
            showToast({
                message: 'A webhook was added after you left, so its secret wasn\'t shown. Replace its secret to get one.',
                durationSecs: 10
            });
            return;
        }
        modal.value = null;
        if (isWebhook(data?.webhook) && !WEBHOOK_FORMATS[data.webhook.format].secretKind) {
            // Postmark endpoints have no signing secret.
            showToast({ message: 'Webhook added', durationSecs: 3 });
        } else if (isWebhook(data?.webhook) && holdSecret('Signing secret created', data.webhook.endpoint, data.webhook.format, data.secret)) {
            showToast({ message: 'Webhook added', durationSecs: 3 });
        } else {
            actionError.value = {
                kind: 'failed',
                message: 'The webhook was added, but Thinpost didn\'t return its secret. If it is listed, replace its secret to get one.'
            };
        }
        void reloadWebhooks();
    } catch (err) {
        if (isUnmounted()) return;
        modalError.value = describeCreateError(err);
        // Unknown outcome: the webhook may exist. Refresh the list and keep Add blocked until it loads, so a retry cannot duplicate it.
        const status = apiErrorStatus(err);
        if (status === undefined || status >= 500) {
            modal.value = null;
            actionError.value = modalError.value;
            modalError.value = null;
            await reloadWebhooks();
        }
    } finally {
        isSubmitting.value = false;
        creating.value = false;
    }
}

function describeCreateError(err: unknown): ApiErrorDescription {
    const status = apiErrorStatus(err);
    if (status === 400)
        return {
            kind: 'invalid',
            status,
            message: 'Thinpost rejected the webhook. Check the URL, description and events.'
        };
    const description = describeApiError(err, {
        mutation: true,
        notFound: 'The app was not found, or it is paused.',
        conflict: '10 webhooks are already enabled. Disable one first.',
        unavailable: UNAVAILABLE
    });
    if (status === undefined || (status !== undefined && status >= 500)) {
        return {
            ...description,
            message: `${description.message.replace(UNKNOWN_OUTCOME, '')} The list was reloaded. If the webhook is listed, replace its secret to get one, and don't add it again.`
        };
    }
    return description;
}

async function update(hook: Webhook, values: WebhookEndpointSubmission) {
    const id = appId.value;
    const seq = loadSeq;
    const body: {
        endpoint?: string;
        description?: string;
        events?: WebhookEventType[];
        auth?: WebhookAuthInputWritable;
    } = {};
    if (values.endpoint !== hook.endpoint) body.endpoint = values.endpoint;
    if (values.description !== hook.description) body.description = values.description;
    const previous = hook.events ?? [];
    if (values.events.length !== previous.length || values.events.some(o => !previous.includes(o))) body.events = values.events;
    if (values.auth) body.auth = values.auth;
    if (!Object.keys(body).length) {
        modal.value = null;
        return;
    }
    isSubmitting.value = true;
    modalError.value = null;
    try {
        const response = await DefaultApi.updateAppWebhook({
            path: { appId: id, webhookId: hook.id },
            body
        });
        if (isUnmounted() || seq !== loadSeq) return;
        if (!isWebhook(response?.data) || response.data.id !== hook.id) throw new InvalidResponseError();
        modal.value = null;
        showToast({ message: 'Webhook updated', durationSecs: 3 });
        void reloadWebhooks();
    } catch (err) {
        if (isUnmounted() || seq !== loadSeq) return;
        modalError.value =
            apiErrorStatus(err) === 400
                ? {
                      kind: 'invalid',
                      status: 400,
                      message: 'Thinpost rejected the changes. Check the URL, description and events.'
                  }
                : describeApiError(err, EDIT_ERRORS);
        if (apiErrorStatus(err) === 404) void reloadWebhooks();
    } finally {
        isSubmitting.value = false;
    }
}

// ---- row actions ----

/** Runs one row mutation at a time; the caller checks `canAct` and confirms first. */
async function mutate(hook: Webhook, action: BusyAction, run: (id: string) => Promise<void>, errors = ERRORS[action]) {
    const id = appId.value;
    const seq = loadSeq;
    busy.value = { id: hook.id, action };
    actionError.value = null;
    try {
        await run(id);
        if (isUnmounted() || seq !== loadSeq) return;
    } catch (err) {
        if (isUnmounted() || seq !== loadSeq) return;
        const description = describeApiError(err, errors);
        const status = apiErrorStatus(err);
        actionError.value =
            action === 'rotate' && (status === undefined || status >= 500)
                ? {
                      ...description,
                      message: `${description.message} If the secret was replaced, replace it again to get the new one.`
                  }
                : description;
    } finally {
        busy.value = null;
    }
    if (seq === loadSeq && !isUnmounted()) await reloadWebhooks();
}

async function sendTest(hook: Webhook) {
    if (!canAct.value) return;
    testResult.value = null;
    await mutate(hook, 'test', async id => {
        const response = await DefaultApi.testAppWebhook({
            path: { appId: id, webhookId: hook.id }
        });
        const result = response?.data;
        if (!result || typeof result.success !== 'boolean' || typeof result.http_status !== 'number') throw new InvalidResponseError();
        if (isUnmounted() || id !== appId.value) return;
        testResult.value = describeTest(hook.endpoint, result);
    });
}

const TEST_FAILURES: Record<string, string> = {
    blocked_destination: 'not a public internet address',
    resolution_failed: 'the host name couldn\'t be found',
    connection_failed: 'it couldn\'t connect',
    tls_failed: 'the secure (TLS) connection failed',
    timeout: 'the request timed out'
};

function describeTest(endpoint: string, result: WebhookTestResult): { tone: 'success' | 'warning'; message: string } {
    if (result.success)
        return {
            tone: 'success',
            message: `Test sent to ${endpoint}. It answered with status ${result.http_status}, so it was accepted.`
        };
    if (result.http_status > 0)
        return {
            tone: 'warning',
            message: `Test sent to ${endpoint}. It answered with status ${result.http_status}, so it was not accepted.`
        };
    const reason = result.failure ? (TEST_FAILURES[result.failure] ?? result.failure) : 'no response';
    return {
        tone: 'warning',
        message: `Test couldn't reach ${endpoint}: ${reason}.`
    };
}

async function enable(hook: Webhook) {
    if (!canAct.value || !app.value.active) return;
    await mutate(hook, 'toggle', async id => {
        const response = await DefaultApi.updateAppWebhook({
            path: { appId: id, webhookId: hook.id },
            body: { active: true }
        });
        if (response?.data?.id !== hook.id || response.data.active !== true) throw new InvalidResponseError();
        if (!isUnmounted() && id === appId.value) showToast({ message: 'Webhook enabled', durationSecs: 3 });
    });
}

async function disable(hook: Webhook) {
    if (!canAct.value) return;
    const seq = loadSeq;
    const confirmed = await showConfirmDestroy(
        'Disable webhook',
        `Disable the webhook to ${hook.endpoint}? New events won't be queued for it, and events already queued wait until you enable it again. An event already on its way may still arrive.`
    );
    if (!confirmed || isUnmounted() || seq !== loadSeq || !canAct.value) return;
    await mutate(hook, 'toggle', async id => {
        const response = await DefaultApi.updateAppWebhook({
            path: { appId: id, webhookId: hook.id },
            body: { active: false }
        });
        if (response?.data?.id !== hook.id || response.data.active !== false) throw new InvalidResponseError();
        if (!isUnmounted() && id === appId.value) {
            showToast({
                message: 'Webhook disabled. An event already on its way may still arrive.',
                durationSecs: 5
            });
        }
    });
}

async function rotate(hook: Webhook) {
    if (!canAct.value) return;
    const seq = loadSeq;
    const confirmed = await showConfirm(
        'Replace signing secret',
        `Replace the signing secret for ${hook.endpoint}? The current secret stops working immediately, so your receiver will reject events until you update it with the new one. Events are retried for about 20 hours, then can be resent. The new secret is shown once.`
    );
    if (!confirmed || isUnmounted() || seq !== loadSeq || !canAct.value) return;
    await mutate(hook, 'rotate', async id => {
        const response = await DefaultApi.rotateAppWebhookSecret({
            path: { appId: id, webhookId: hook.id }
        });
        const data = response?.data;
        if (isUnmounted() || id !== appId.value) {
            // The secret is dropped, never displayed later.
            showToast({
                message: 'A secret was replaced after you left and wasn\'t shown. Replace it again to get a new one.',
                durationSecs: 10
            });
            return;
        }
        if (!holdSecret('Signing secret replaced', hook.endpoint, hook.format, data?.secret)) throw new InvalidResponseError();
    });
}

async function remove(hook: Webhook) {
    if (!canAct.value) return;
    const seq = loadSeq;
    const confirmed = await showConfirmDestroy(
        'Delete webhook',
        `Delete ${hook.endpoint}? Queued events are cancelled and won't be sent. An event already on its way may still arrive. Webhook event history is kept. This can't be undone.`
    );
    if (!confirmed || isUnmounted() || seq !== loadSeq || !canAct.value) return;
    await mutate(hook, 'delete', async id => {
        await DefaultApi.deleteAppWebhook({
            path: { appId: id, webhookId: hook.id }
        });
        if (!isUnmounted() && id === appId.value) showToast({ message: 'Webhook deleted', durationSecs: 3 });
    });
}
</script>

<style lang="scss" scoped>
#app-webhooks {
    display: flex;
    flex-direction: column;
    gap: 16px;
}

.webhooks-body {
    display: flex;
    flex-direction: column;
    gap: 12px;
}

/* Hide the body when every child rendered nothing (only v-if comments remain). */
.webhooks-body.webhooks-body:not(:has(*)) {
    display: none;
}

.explainer {
    display: flex;
    flex-direction: column;
    gap: 8px;

    ul {
        display: flex;
        flex-direction: column;
        gap: 4px;
    }
}

.endpoint-cell {
    min-width: 256px;
    font-weight: 400;

    > * {
        display: block;
    }
}

.endpoint {
    font-weight: 600;
    overflow-wrap: anywhere;
}

.description {
    margin-top: 2px;
    color: var(--text-2);
    overflow-wrap: anywhere;
}

.webhook-auth,
.webhook-warn {
    margin-top: 2px;
    color: var(--text-3);
    font-size: 12px;
}

.webhook-warn {
    color: var(--warning, var(--text-2));
}

.muted {
    color: var(--text-3);
}

.webhook-id {
    margin-top: 4px;
    color: var(--text-3);
    font-size: 12px;
    overflow-wrap: anywhere;
}

.events {
    white-space: nowrap;
}

.actions {
    text-align: right;
    white-space: nowrap;
}

.action-buttons {
    display: inline-flex;
    align-items: center;
    gap: 6px;
}
</style>
