<template>
    <VfModal class="tp-modal" role="dialog" aria-modal="true" :aria-labelledby="`${id}-title`" close-on-mask-click @close="emit('close')" @form-submit="submit">
        <template #header>
            <span :id="`${id}-title`">{{ initial ? 'Edit webhook' : 'Add webhook' }}</span>
        </template>

        <div class="modal-body">
            <ApiErrorNotice v-if="error" :error="error" />

            <div class="label-row">
                <label :for="`${id}-format`">Format</label>
                <HelpButton topic="webhook formats">
                    <ul>
                        <li><strong>Thinpost:</strong> Thinpost's own JSON, signed with an HMAC-SHA256 <code>tpwh_</code> secret.</li>
                        <li><strong>Postmark:</strong> Postmark webhook JSON. Postmark doesn't sign events, so use Basic auth or a header to check where they came from.</li>
                        <li><strong>Resend:</strong> Resend/Svix JSON, signed with a <code>whsec_</code> secret.</li>
                        <li><strong>Mailgun:</strong> Mailgun webhook JSON. The signature in the body doesn't cover the event data.</li>
                        <li><strong>Inbound mail (Thinpost):</strong> Sends each email received at your receiving domains, not events about mail you sent. It is signed like Thinpost events.</li>
                    </ul>
                    <p>
                        Postmark, Resend and Mailgun formats leave out details Thinpost doesn't record, such as the From address and tags. Subjects of
                        messages sent with Enhanced Protection are never included. You can't change the format after the webhook is added.
                    </p>
                </HelpButton>
            </div>
            <p v-if="initial" :id="`${id}-format`" class="format-fixed">
                <span class="tag">{{ WEBHOOK_FORMATS[format].label }}</span>
            </p>
            <select v-else :id="`${id}-format`" v-model="format" class="input">
                <option v-for="name in WEBHOOK_FORMAT_ORDER" :key="name" :value="name">
                    {{ WEBHOOK_FORMATS[name].label }}
                </option>
            </select>

            <div v-if="formatInfo.receivesMail" class="notice" role="note" data-testid="inbound-format-help">
                <i class="fa-solid fa-circle-info" aria-hidden="true" />
                <ul class="notice-body">
                    <li>Each received email is sent to this URL as one request, once for every address it was sent to. It includes the full message as received, so a request can be several megabytes.</li>
                    <li>Your server has 2 minutes to answer. Reply with a success response once the message is safely stored, and Thinpost stops retrying. Anything else is retried for about 20 hours.</li>
                    <li>Each request is signed with the signing secret. Check the signature before trusting it. A request may arrive more than once, so ignore repeats.</li>
                    <li>Mail to your receiving domains waits at the sender until at least one inbound mail webhook is enabled.</li>
                </ul>
            </div>

            <label :for="`${id}-endpoint`">Webhook URL</label>
            <input
                :id="`${id}-endpoint`"
                v-model="endpoint"
                v-autofocus
                class="input mono"
                type="text"
                inputmode="url"
                autocomplete="off"
                autocapitalize="off"
                spellcheck="false"
                placeholder="https://hooks.example.com/thinpost"
                required
                :aria-invalid="!!endpointError && touched"
                :aria-describedby="`${id}-endpoint-hint`"
                @blur="touched = true"
            />
            <p :id="`${id}-endpoint-hint`" class="hint" :class="{ invalid: endpointError && touched }">
                {{
                    endpointError && touched ? endpointError : movedNote ? 'The user name and password were moved to Basic authentication.' : 'An https:// address on a public host, port 443.'
                }}
            </p>
            <p v-if="initial" class="hint">Events already queued will use the new URL on their next attempt.</p>

            <label :for="`${id}-description`">Description <span class="optional">(optional)</span></label>
            <input
                :id="`${id}-description`"
                v-model="description"
                class="input"
                type="text"
                autocomplete="off"
                spellcheck="false"
                :aria-invalid="!!descriptionError"
                :aria-describedby="`${id}-description-hint`"
            />
            <p :id="`${id}-description-hint`" class="hint" :class="{ invalid: descriptionError }">
                {{ descriptionError ?? 'A note to help you recognize it. Up to 256 characters; accented and non-Latin characters count as more than one.' }}
            </p>

            <fieldset v-if="!formatInfo.receivesMail" class="events" :aria-describedby="`${id}-events-hint`">
                <legend>Events</legend>
                <label>
                    <input v-model="scope" type="radio" :name="`${id}-scope`" value="all" />
                    <span>All events</span>
                </label>
                <label>
                    <input v-model="scope" type="radio" :name="`${id}-scope`" value="selected" />
                    <span>Selected events</span>
                </label>
            </fieldset>
            <div v-if="scope === 'selected' && !formatInfo.receivesMail" class="groups">
                <fieldset v-for="group in formatInfo.groups" :key="group.label" class="group">
                    <legend v-if="formatInfo.groups.length > 1">{{ group.label }}</legend>
                    <label v-for="event in group.events" :key="event">
                        <input v-model="selected" type="checkbox" :value="event" />
                        <code>{{ event }}</code>
                    </label>
                </fieldset>
            </div>
            <p v-if="eventsError && touched && !formatInfo.receivesMail" :id="`${id}-events-hint`" class="hint invalid">
                {{ eventsError }}
            </p>

            <div class="label-row">
                <button type="button" class="disclosure" :aria-expanded="authOpen" :aria-controls="`${id}-auth`" @click="authOpen = !authOpen">
                    <i class="fa-solid" :class="authOpen ? 'fa-chevron-down' : 'fa-chevron-right'" aria-hidden="true" />
                    HTTP authentication <span class="optional">(optional)</span>
                </button>
                <HelpButton topic="HTTP authentication">
                    <p>Thinpost sends this Basic user name and password, and these headers, with every event it sends to this webhook.</p>
                    <p>
                        Passwords and header values can't be viewed again once saved. When editing, leave them blank to keep them. If you change the
                        user name, a header name or any value, you need to enter every password and value again.
                    </p>
                </HelpButton>
            </div>
            <p v-if="unsignedWarning" class="notice warning" role="status">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <span class="notice-body">Unsigned. Add Basic auth or a header so your receiver can trust it.</span>
            </p>

            <div v-if="authOpen" :id="`${id}-auth`" class="auth">
                <template v-if="removeAuth">
                    <p class="hint">Authentication will be removed when you save.</p>
                    <button type="button" class="btn sm" @click="removeAuth = false">Keep authentication</button>
                </template>
                <template v-else>
                    <div class="auth-row">
                        <div>
                            <label :for="`${id}-user`">User name</label>
                            <input
                                :id="`${id}-user`"
                                v-model="basicUser"
                                class="input"
                                type="text"
                                autocomplete="off"
                                autocapitalize="off"
                                spellcheck="false"
                                :aria-invalid="!!shownAuthErrors.username"
                            />
                            <p v-if="shownAuthErrors.username" class="hint invalid">
                                {{ shownAuthErrors.username }}
                            </p>
                        </div>
                        <div>
                            <label :for="`${id}-pass`">Password</label>
                            <input
                                :id="`${id}-pass`"
                                v-model="basicPass"
                                class="input"
                                type="password"
                                autocomplete="new-password"
                                :placeholder="initial?.auth?.username ? 'Unchanged' : ''"
                                :aria-invalid="!!shownAuthErrors.password"
                            />
                            <p v-if="shownAuthErrors.password" class="hint invalid">
                                {{ shownAuthErrors.password }}
                            </p>
                        </div>
                    </div>

                    <div v-for="(row, index) in headerRows" :key="row.key" class="auth-row header-row">
                        <div>
                            <label :for="`${id}-hn-${row.key}`">Header {{ index + 1 }} name</label>
                            <input
                                :id="`${id}-hn-${row.key}`"
                                v-model="row.name"
                                class="input mono"
                                type="text"
                                autocomplete="off"
                                autocapitalize="off"
                                spellcheck="false"
                                placeholder="X-Api-Key"
                                :aria-invalid="!!shownAuthErrors.headers[index]"
                            />
                        </div>
                        <div>
                            <label :for="`${id}-hv-${row.key}`">Value</label>
                            <input
                                :id="`${id}-hv-${row.key}`"
                                v-model="row.value"
                                class="input mono"
                                type="password"
                                autocomplete="new-password"
                                :placeholder="row.stored ? 'Unchanged' : ''"
                                :aria-invalid="!!shownAuthErrors.headers[index]"
                            />
                        </div>
                        <button type="button" class="btn sm" :aria-label="`Remove header ${index + 1}`" @click="removeHeader(index)">
                            <i class="fa-solid fa-xmark" aria-hidden="true" />
                        </button>
                        <p v-if="shownAuthErrors.headers[index]" class="hint invalid">
                            {{ shownAuthErrors.headers[index] }}
                        </p>
                    </div>

                    <div class="auth-actions">
                        <button type="button" class="btn sm" :disabled="headerRows.length >= MAX_WEBHOOK_HEADERS" @click="addHeader">
                            <i class="fa-solid fa-plus" aria-hidden="true" />
                            Add header
                        </button>
                        <button v-if="initial?.auth" type="button" class="btn sm danger" @click="removeAuth = true">Remove authentication</button>
                    </div>
                </template>
            </div>
        </div>

        <template #footer>
            <button type="button" class="btn" @click="emit('close')">Cancel</button>
            <button type="submit" class="btn primary" :disabled="isSubmitting">
                {{ isSubmitting ? 'Saving…' : initial ? 'Save changes' : 'Add webhook' }}
            </button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, useId, watch } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import type { WebhookAuthInputWritable, WebhookEventType, WebhookFormat } from '@/openapi-client-generated';
import { useModalPresence } from '@/shared/modal-state';
import {
    MAX_WEBHOOK_HEADERS,
    splitEndpointUserinfo,
    validateWebhookBasic,
    validateWebhookDescription,
    validateWebhookEndpoint,
    validateWebhookHeader
} from '@/validation';
import { WEBHOOK_FORMAT_ORDER, WEBHOOK_FORMATS } from '@/webhook-events';

import ApiErrorNotice from './api-error-notice.vue';
import HelpButton from './help-button.vue';

export interface WebhookEndpointValues {
    format: WebhookFormat;
    endpoint: string;
    description: string;
    /** Selected event types; an empty list means all events. */
    events: WebhookEventType[];
    /** Stored authentication names (values are never returned). */
    auth?: { username?: string; headerNames: string[] };
}

/** What the form submits: the values plus authentication only when it was set, replaced or removed (`{}` removes). */
export type WebhookEndpointSubmission = Omit<WebhookEndpointValues, 'auth'> & {
    auth?: WebhookAuthInputWritable;
};

const props = defineProps<{
    /** Present when editing; prefills the form. */
    initial?: WebhookEndpointValues;
    /** The format chosen when adding; the select stays editable. Ignored when editing. */
    presetFormat?: WebhookFormat;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
}>();

const emit = defineEmits<{
    close: [];
    submit: [values: WebhookEndpointSubmission];
}>();

interface HeaderRow {
    key: number;
    name: string;
    value: string;
    /** A stored header whose value is kept while the field is blank. */
    stored: boolean;
}

useModalPresence();
const id = useId();
const format = ref<WebhookFormat>(props.initial?.format ?? props.presetFormat ?? 'thinpost');
const endpoint = ref(props.initial?.endpoint ?? '');
const description = ref(props.initial?.description ?? '');
const scope = ref<'all' | 'selected'>(props.initial?.events.length ? 'selected' : 'all');
const selected = ref<WebhookEventType[]>([...(props.initial?.events ?? [])]);
const touched = ref(false);
const movedNote = ref(false);

let rowKey = 0;
const basicUser = ref(props.initial?.auth?.username ?? '');
const basicPass = ref('');
const headerRows = ref<HeaderRow[]>(
    (props.initial?.auth?.headerNames ?? []).map(name => ({
        key: rowKey++,
        name,
        value: '',
        stored: true
    }))
);
const authOpen = ref(!!props.initial?.auth);
const removeAuth = ref(false);

const formatInfo = computed(() => WEBHOOK_FORMATS[format.value]);

// Switching format resets the event filter, since each format has its own event types.
watch(format, () => {
    scope.value = 'all';
    selected.value = [];
});

// A pasted https://user:pass@host/... moves its credentials into the Basic fields; user information is never sent.
watch(endpoint, value => {
    const split = splitEndpointUserinfo(value);
    if (!split || !split.endpoint.slice('https://'.length)) return;
    endpoint.value = split.endpoint;
    basicUser.value = split.username;
    basicPass.value = split.password;
    authOpen.value = true;
    removeAuth.value = false;
    movedNote.value = true;
});

onBeforeUnmount(() => {
    // Credentials do not outlive the form.
    basicPass.value = '';
    for (const row of headerRows.value) row.value = '';
});

function addHeader() {
    if (headerRows.value.length < MAX_WEBHOOK_HEADERS)
        headerRows.value.push({
            key: rowKey++,
            name: '',
            value: '',
            stored: false
        });
}

function removeHeader(index: number) {
    headerRows.value.splice(index, 1);
}

const activeRows = computed(() => headerRows.value.filter(row => row.name || row.value));

/** Whether the credentials differ from what is stored (always true for a new endpoint that has any). */
const authDirty = computed(() => {
    const initialAuth = props.initial?.auth;
    if (!initialAuth) return !!(basicUser.value || basicPass.value || activeRows.value.length);
    const names = activeRows.value.map(row => row.name);
    return (
        basicUser.value !== (initialAuth.username ?? '') ||
        !!basicPass.value ||
        activeRows.value.some(row => row.value) ||
        names.length !== initialAuth.headerNames.length ||
        names.some((name, i) => name !== initialAuth.headerNames[i])
    );
});

const authErrors = computed(() => {
    const none = {
        username: null,
        password: null,
        headers: [] as Array<string | null>
    };
    if (removeAuth.value || !authDirty.value) return none;
    const basic = validateWebhookBasic(basicUser.value, basicPass.value);
    const basicSet = !!basicUser.value;
    const seen = new Set<string>();
    const headers = headerRows.value.map(row => {
        if (!row.name && !row.value) return null;
        const problem = validateWebhookHeader(row.name, row.value, basicSet);
        if (problem) return problem;
        const lower = row.name.toLowerCase();
        if (seen.has(lower)) return 'Duplicate header name.';
        seen.add(lower);
        return null;
    });
    return { ...basic, headers };
});

const hasAuthError = computed(() => !!authErrors.value.username || !!authErrors.value.password || authErrors.value.headers.some(Boolean));

// Field messages wait until the form has been submitted or the endpoint field left, so an untouched form is not flagged.
const shownAuthErrors = computed(() => (touched.value ? authErrors.value : { username: null, password: null, headers: [] as Array<string | null> }));

/** True when a Postmark endpoint will have no authentication after saving. */
const unsignedWarning = computed(() => {
    if (format.value !== 'postmark') return false;
    if (removeAuth.value) return true;
    if (props.initial?.auth) return !basicUser.value && !activeRows.value.length;
    return !basicUser.value && !basicPass.value && !activeRows.value.length;
});

const endpointError = computed(() => validateWebhookEndpoint(endpoint.value));
const descriptionError = computed(() => validateWebhookDescription(description.value));
const eventsError = computed(() => (!formatInfo.value.receivesMail && scope.value === 'selected' && !selected.value.length ? 'Select at least one event type, or choose All events.' : null));

function buildAuth(): WebhookAuthInputWritable | undefined {
    if (removeAuth.value) return {};
    if (!authDirty.value) return undefined;
    const rows = activeRows.value;
    return {
        ...(basicUser.value ? { username: basicUser.value, password: basicPass.value } : {}),
        ...(rows.length ? { headers: rows.map(row => ({ name: row.name, value: row.value })) } : {})
    };
}

function submit() {
    touched.value = true;
    if (hasAuthError.value) authOpen.value = true;
    if (endpointError.value || descriptionError.value || eventsError.value || hasAuthError.value || props.isSubmitting) return;
    const ordered = formatInfo.value.groups.flatMap(group => group.events).filter(event => selected.value.includes(event));
    const auth = buildAuth();
    emit('submit', {
        format: format.value,
        endpoint: endpoint.value,
        description: description.value,
        events: scope.value === 'selected' && !formatInfo.value.receivesMail ? ordered : [],
        ...(auth ? { auth } : {})
    });
    // Values are cleared as soon as they are handed over.
    basicPass.value = '';
    for (const row of headerRows.value) row.value = '';
}
</script>

<style lang="scss" scoped>
.modal-body {
    gap: 8px;
    width: min(560px, calc(100vw - 72px));

    > label {
        margin-top: 4px;
        color: var(--text);
        font-weight: 600;
    }

    .hint.invalid {
        color: var(--danger);
    }
}

.label-row {
    display: flex;
    align-items: center;
    gap: 6px;
    margin-top: 4px;

    label {
        color: var(--text);
        font-weight: 600;
    }
}

.format-fixed {
    margin: 0;
}

.disclosure {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 0;
    border: 0;
    background: none;
    color: var(--text);
    font: inherit;
    font-weight: 600;
    cursor: pointer;

    i {
        width: 12px;
        font-size: 11px;
        color: var(--text-3);
    }
}

.notice {
    margin: 0;
}

.auth {
    display: flex;
    flex-direction: column;
    gap: 8px;
    padding: 12px;
    border: 1px solid var(--border);
    border-radius: 6px;

    .input {
        width: 100%;
    }

    label {
        display: block;
        margin-bottom: 4px;
        color: var(--text-2);
        font-size: 12px;
        font-weight: 600;
    }
}

.auth-row {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 8px;

    > div {
        min-width: 0;
    }
}

.header-row {
    grid-template-columns: 1fr 1fr auto;
    align-items: end;

    .hint {
        grid-column: 1 / -1;
    }
}

.auth-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
}

.optional {
    color: var(--text-3);
    font-weight: 400;
}

fieldset {
    display: flex;
    flex-wrap: wrap;
    gap: 8px 20px;
    margin: 0;
    padding: 0;
    border: 0;

    legend {
        margin-bottom: 8px;
        padding: 0;
        color: var(--text);
        font-weight: 600;
    }

    label {
        display: flex;
        align-items: center;
        gap: 8px;
        cursor: pointer;
    }
}

.events {
    margin-top: 4px;
}

.groups {
    display: flex;
    flex-direction: column;
    gap: 12px;
}

.group {
    legend {
        font-size: 12px;
        color: var(--text-3);
    }

    code {
        font-size: 12px;
    }
}

@media (max-width: 480px) {
    .auth-row,
    .header-row {
        grid-template-columns: 1fr;
    }
}
</style>
