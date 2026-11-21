<template>
    <div id="app-smtp" class="smtp-page">
        <div v-if="isUnavailable" class="card empty" data-testid="smtp-unavailable">
            <i class="fa-solid fa-paper-plane" aria-hidden="true" />
            <h2>SMTP isn't turned on yet</h2>
            <p class="hint">This Thinpost server doesn't accept mail over SMTP yet. Ask your Thinpost administrator when it will.</p>
        </div>

        <div v-else-if="isLoading && !data" class="card shell-loading">
            <Loader label="Loading SMTP settings" />
        </div>

        <ApiErrorNotice v-else-if="loadError && !data" :error="loadError" :retry="load" />

        <template v-else-if="data">
            <ApiErrorNotice v-if="loadError" :error="loadError" :retry="load" />

            <section aria-labelledby="smtp-connect-heading" class="card" data-testid="smtp-connect">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="smtp-connect-heading">How to connect</h2>
                        <HelpButton topic="connecting over SMTP">
                            <p>
                                Any software that can send email through an SMTP server can send through Thinpost: printers, scanners, CRMs, WordPress and
                                other apps that have an email or SMTP setting. Give it the server name, a port, and one of this app's SMTP credentials.
                            </p>
                            <ul>
                                <li><strong>Username and password.</strong> Create a credential below. Its password is shown once, when you create it.</li>
                                <li><strong>Encryption.</strong> Choose SSL/TLS on port 465 if your software offers it. On port 587, set the encryption to STARTTLS and make it required, not optional.</li>
                                <li>Never use an unencrypted connection. Your password would travel in the open.</li>
                                <li>Credentials only send mail. They can't read messages or change anything in the console.</li>
                            </ul>
                        </HelpButton>
                    </div>
                </div>
                <div class="card-body connect-body">
                    <div v-if="hostStatus === 'not_reported'" class="notice warning" data-testid="smtp-host-not-reported">
                        <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                        <p class="notice-body">
                            <strong>No server name yet.</strong> The sending worker hasn't connected yet, so Thinpost doesn't have a server name to give you. Try again in a few minutes, or ask your Thinpost administrator.
                        </p>
                    </div>
                    <div v-else-if="hostStatus === 'dns_unconfirmed'" class="notice warning" data-testid="smtp-host-dns-unconfirmed">
                        <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                        <p class="notice-body">
                            <strong>Server name can't be confirmed yet.</strong> Thinpost only shows a server name once the internet's name records (DNS) point it at the right address, and that
                            check didn't pass just now. It may still be updating. Try again in a few minutes, or ask your Thinpost administrator.
                        </p>
                    </div>
                    <div v-else-if="hostStatus === 'ip_unavailable'" class="notice warning" data-testid="smtp-host-ip-unavailable">
                        <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                        <p class="notice-body">
                            <strong>No server name for this app's IP.</strong> The sending IP chosen for this app is no longer on the sending worker, so its mail is waiting.
                            Choose another IP for the app on its <RouterLink :to="{ name: 'app-keys', params: { appId } }" data-testid="smtp-host-ip-link">Keys page</RouterLink>.
                        </p>
                    </div>
                    <div v-else-if="!hostName" class="notice warning" data-testid="smtp-host-missing">
                        <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                        <p class="notice-body">
                            <strong>No server name yet.</strong> Thinpost doesn't know which server name to give you for this app. Ask your Thinpost administrator.
                        </p>
                    </div>
                    <CopyField v-else label="Server" :value="hostName" />

                    <ul class="ports" aria-label="Ports">
                        <li v-for="item in SMTP_PORTS" :key="item.port" class="port" :data-testid="`smtp-port-${item.port}`">
                            <CopyField :label="item.label" :value="String(item.port)" />
                            <p class="hint port-note">
                                {{ item.note }}
                                <span class="tag" :class="availabilityTag(item.port).tone" :title="availabilityTag(item.port).title" :data-testid="`smtp-port-${item.port}-status`">
                                    {{ availabilityTag(item.port).text }}
                                </span>
                            </p>
                        </li>
                    </ul>

                    <p class="hint" data-testid="smtp-recommendation">
                        <strong>Recommended:</strong> port 465. If your software can only use 587, set its encryption to STARTTLS and make it <em>required</em>.
                    </p>
                    <p class="hint">The username and password are the ones you create under Credentials below.</p>
                </div>
            </section>

            <section aria-labelledby="smtp-credentials-heading" class="card" data-testid="smtp-credentials">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="smtp-credentials-heading">Credentials</h2>
                        <HelpButton topic="SMTP credentials">
                            <p>Each credential is a username and password for one device or service, so you can turn one off without touching the others.</p>
                            <ul>
                                <li>A password is shown once. If it's lost, create a new credential and revoke the old one.</li>
                                <li>Revoking stops a credential from signing in. Messages it already sent are kept and still show its name.</li>
                                <li>An app can have up to {{ MAX_ACTIVE_CREDENTIALS }} active credentials.</li>
                            </ul>
                        </HelpButton>
                    </div>
                    <button type="button" class="btn primary" :disabled="!canIssue" :aria-describedby="issueHint ? 'smtp-issue-hint' : undefined" @click="openIssue">
                        <i class="fa-solid fa-key" aria-hidden="true" />
                        Create credential
                    </button>
                </div>

                <div v-if="issueHint || actionError" class="card-body smtp-body">
                    <p v-if="issueHint" id="smtp-issue-hint" class="hint">{{ issueHint }}</p>
                    <ApiErrorNotice v-if="actionError" :error="actionError" />
                </div>

                <div v-if="!data.credentials.length" class="card-body empty">
                    <i class="fa-solid fa-key" aria-hidden="true" />
                    <h2>No SMTP credentials yet</h2>
                </div>
                <div v-else class="table-wrap" :aria-busy="isLoading">
                    <table class="data">
                        <caption class="sr-only">SMTP credentials for {{ app.name }}</caption>
                        <thead>
                            <tr>
                                <th scope="col">Name</th>
                                <th scope="col">Status</th>
                                <th scope="col">Created</th>
                                <th scope="col">Last used</th>
                                <th scope="col">Username</th>
                                <th scope="col"><span class="sr-only">Actions</span></th>
                            </tr>
                        </thead>
                        <tbody>
                            <tr v-for="credential in data.credentials" :key="credential.id" :class="{ inactive: !credential.active }">
                                <th scope="row" class="name">{{ credential.name }}</th>
                                <td>
                                    <span class="tag" :class="{ success: credential.active }">{{ credential.active ? 'Active' : 'Revoked' }}</span>
                                </td>
                                <td>
                                    <time :datetime="credential.created_at">{{ formatDateTime(credential.created_at) }}</time>
                                </td>
                                <td>
                                    <time v-if="credential.last_used_at" :datetime="credential.last_used_at">{{ formatDateTime(credential.last_used_at) }}</time>
                                    <span v-else class="muted">Never</span>
                                </td>
                                <td>
                                    <code class="id">{{ credential.username }}</code>
                                </td>
                                <td class="actions">
                                    <button
                                        v-if="credential.active"
                                        type="button"
                                        class="btn sm danger"
                                        :disabled="!!revokingId || authState.status !== 'signed-in'"
                                        @click="revoke(credential)"
                                    >
                                        {{ revokingId === credential.id ? 'Revoking…' : 'Revoke' }}
                                        <span class="sr-only">{{ credential.name }}</span>
                                    </button>
                                </td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </section>

            <section aria-labelledby="smtp-from-heading" class="card" data-testid="smtp-from" :aria-busy="isSaving">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="smtp-from-heading">From address</h2>
                        <HelpButton topic="the From address">
                            <p>
                                The From address is who the message says it's from. Mailbox providers only trust mail sent from domains you've verified,
                                but many devices and apps send from addresses of their own, such as <code>scanner@office.example.net</code>. This setting decides what happens then.
                            </p>
                            <ul>
                                <li><strong>Only accept verified addresses.</strong> Mail from any other address is refused, so the sender sees an error.</li>
                                <li><strong>Send from one of my domains.</strong> The address is moved onto a domain you pick, so <code>scanner@office.example.net</code> becomes <code>scanner=office.example.net@example.com</code>. The original address stays in the name part, so you can still tell senders apart. Very long or unusual names are shortened and end with a short code.</li>
                                <li><strong>Always send from one address.</strong> Every message goes out from the address you enter.</li>
                                <li>The address the sender used is kept and shown as “Original From” on the message.</li>
                            </ul>
                        </HelpButton>
                    </div>
                </div>
                <form class="card-body from-body" novalidate @submit.prevent="save">
                    <ApiErrorNotice v-if="domainsError" :error="domainsError" :retry="loadDomains" retry-label="Reload domains" />
                    <p v-else-if="domains && !domains.length" class="hint" data-testid="smtp-no-domains">
                        This app has no verified sending domain yet.
                        <RouterLink :to="{ name: 'app-domains', params: { appId } }">Go to Sending domains</RouterLink>
                    </p>

                    <fieldset class="modes" :disabled="isSaving">
                        <legend class="sr-only">What to do with the From address</legend>
                        <label class="mode">
                            <input v-model="form.mode" type="radio" name="smtp-from-mode" value="off" data-testid="smtp-mode-off" />
                            <span><strong>Only accept verified addresses</strong><span class="hint">Mail from any other address is refused.</span></span>
                        </label>
                        <label class="mode">
                            <input v-model="form.mode" type="radio" name="smtp-from-mode" value="domain" data-testid="smtp-mode-domain" />
                            <span><strong>Send from one of my domains</strong><span class="hint">Move other addresses onto a domain you choose.</span></span>
                        </label>
                        <label class="mode">
                            <input v-model="form.mode" type="radio" name="smtp-from-mode" value="address" data-testid="smtp-mode-address" />
                            <span><strong>Always send from one address</strong><span class="hint">Every message goes out from the address you enter.</span></span>
                        </label>
                    </fieldset>

                    <div v-if="form.mode === 'domain'" class="field-group">
                        <label for="smtp-from-domain">Domain</label>
                        <select id="smtp-from-domain" v-model="form.domain" class="input" :disabled="isSaving" :aria-invalid="!!validation.domainError && touched" aria-describedby="smtp-from-domain-hint">
                            <option value="" disabled>Choose a domain</option>
                            <option v-for="name in domainChoices" :key="name" :value="name">{{ name }}</option>
                        </select>
                        <p id="smtp-from-domain-hint" class="hint" :class="{ invalid: validation.domainError && touched }">
                            {{ validation.domainError && touched ? validation.domainError : 'Only verified sending domains of this app are listed. The original address is kept in the name part, so senders stay distinguishable.' }}
                        </p>
                    </div>

                    <div v-if="form.mode === 'address'" class="field-group">
                        <label for="smtp-from-address">Address</label>
                        <input
                            id="smtp-from-address"
                            v-model="form.address"
                            class="input"
                            type="text"
                            autocomplete="off"
                            autocapitalize="off"
                            spellcheck="false"
                            placeholder="no-reply@example.com"
                            :disabled="isSaving"
                            :aria-invalid="!!validation.addressError && touched"
                            aria-describedby="smtp-from-address-hint"
                            @blur="touched = true"
                        />
                        <p id="smtp-from-address-hint" class="hint" :class="{ invalid: validation.addressError && touched }">
                            {{ validation.addressError && touched ? validation.addressError : 'It must be on one of this app\'s verified sending domains.' }}
                        </p>
                    </div>

                    <label v-if="form.mode !== 'off'" class="check">
                        <input v-model="form.onlyUnauthorized" type="checkbox" :disabled="isSaving" data-testid="smtp-only-unauthorized" />
                        <span>Leave addresses on my verified domains alone <span class="hint">(only change other addresses)</span></span>
                    </label>

                    <div v-if="examples.length" class="examples" data-testid="smtp-examples">
                        <p class="hint"><strong>For example</strong></p>
                        <ul>
                            <li v-for="example in examples" :key="example.sent">
                                <code>{{ example.sent }}</code> <i class="fa-solid fa-arrow-right" aria-hidden="true" /> <code>{{ example.becomes }}</code>
                            </li>
                        </ul>
                    </div>

                    <div v-if="unsigned.length" class="notice warning" data-testid="smtp-signing-warning">
                        <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                        <p class="notice-body">
                            <strong>Mail from {{ unsigned.join(', ') }} isn't being signed yet.</strong> Without a DKIM signature, mailbox providers are more likely to send it to spam.
                            Ask your Thinpost administrator to finish setting up signing for {{ unsigned.length === 1 ? 'this domain' : 'these domains' }}.
                        </p>
                    </div>

                    <ApiErrorNotice v-if="saveError" :error="saveError" />
                    <p v-if="savedNote" class="saved" role="status"><i class="fa-solid fa-check" aria-hidden="true" /> {{ savedNote }}</p>

                    <div class="form-actions">
                        <button type="submit" class="btn primary" :disabled="!canSave" data-testid="smtp-save">{{ isSaving ? 'Saving…' : 'Save' }}</button>
                        <button v-if="isDirty" type="button" class="btn" :disabled="isSaving" @click="resetForm">Discard changes</button>
                    </div>
                </form>
            </section>
        </template>

        <NameModal
            v-if="isIssueOpen"
            title="Create SMTP credential"
            label="Credential name"
            hint="A name to recognize this credential by, such as the device or service that uses it. Up to 128 characters; accented and non-Latin characters count as more than one."
            submit-label="Create credential"
            :is-submitting="isIssuing"
            :error="issueError"
            @close="closeIssue"
            @submit="issue"
        />

        <SecretModal
            v-if="issued"
            title="SMTP credential created"
            :subject="issued.name"
            :username="issued.username"
            :secret="issued.password"
            secret-label="Password"
            @done="discardSecret"
        >
            Enter these as the username and password in your software's SMTP settings.
        </SecretModal>
    </div>
</template>

<script lang="ts" setup>
import { showConfirmDestroy, showToast } from '@zyno-io/vue-foundation';
import { computed, nextTick, onBeforeUnmount, ref, shallowRef, watch } from 'vue';

import { describeApiError, invalidResponse, type ApiErrorDescription } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { formatDateTime, formatRelativeTime } from '@/format';
import { DefaultApi, type AppSmtp, type SmtpCredential } from '@/openapi-client-generated';
import { loadRuntimeFeatures, runtimeFeatures } from '@/runtime-features';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import CopyField from '@/shared/components/copy-field.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import NameModal from '@/shared/components/name-modal.vue';
import SecretModal from '@/shared/components/secret-modal.vue';
import { useOneTimeSecret } from '@/shared/one-time-secret';
import {
    activeCredentialCount,
    fromExamples,
    formFromPolicy,
    MAX_ACTIVE_CREDENTIALS,
    parseAppSmtp,
    parseSmtpSettings,
    parseIssuedCredential,
    portAvailability,
    SMTP_PORTS,
    unsignedDomains,
    validateFromForm,
    type FromForm
} from '@/smtp';

const { app, appId } = useAppContext();

const data = shallowRef<AppSmtp | null>(null);
const isLoading = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const actionError = ref<ApiErrorDescription | null>(null);

/** Verified sending domains of this app; null until loaded. */
const domains = shallowRef<string[] | null>(null);
const domainsError = ref<ApiErrorDescription | null>(null);

const isIssueOpen = ref(false);
const isIssuing = ref(false);
const issueError = ref<ApiErrorDescription | null>(null);
const revokingId = ref<string | null>(null);

const form = ref<FromForm>({ mode: 'off', domain: '', address: '', onlyUnauthorized: true });
const saved = ref<FromForm>({ ...form.value });
const touched = ref(false);
const isSaving = ref(false);
const saveError = ref<ApiErrorDescription | null>(null);
const savedNote = ref<string | null>(null);

/** Only a server that reports SMTP submission as off hides the screen; an unknown answer lets the API decide. */
const isUnavailable = computed(() => runtimeFeatures.value !== null && !runtimeFeatures.value.smtp_submission);

const {
    held: issued,
    isUnmounted,
    discard: discardSecret
} = useOneTimeSecret<{ name: string; username: string; password: string }>({
    isPending: () => isIssuing.value,
    pendingPrompt: {
        title: 'Leave while a credential is being created?',
        message: 'If the credential is created, its password won\'t be shown and you should revoke the credential.'
    },
    heldPrompt: { title: 'Leave without saving the password?', message: 'The SMTP password is still on screen and can\'t be shown again.' }
});

const activeCount = computed(() => (data.value ? activeCredentialCount(data.value.credentials) : 0));
const issueHint = computed(() => {
    if (!app.value.active) return 'This app is paused, so new SMTP credentials can\'t be created.';
    if (activeCount.value >= MAX_ACTIVE_CREDENTIALS) return `This app has ${MAX_ACTIVE_CREDENTIALS} active credentials, the most it can have. Revoke one you no longer use to create another.`;
    return null;
});
const canIssue = computed(() => !issueHint.value && authState.status === 'signed-in' && !isIssuing.value);

const hostName = computed(() => data.value?.connection['host'] ?? '');
const hostStatus = computed(() => data.value?.connection['host_status']);
const domainChoices = computed(() => {
    const names = [...(domains.value ?? [])];
    // Keep a saved domain that is no longer verified visible so the form shows what is stored.
    if (form.value.domain && !names.includes(form.value.domain)) names.push(form.value.domain);
    return names;
});
const validation = computed(() => validateFromForm(form.value, domains.value ?? []));
const examples = computed(() => fromExamples(form.value, domains.value ?? []));
const unsigned = computed(() => unsignedDomains(data.value?.settings.signing_ready ?? null));
const isDirty = computed(() => JSON.stringify(normalized(form.value)) !== JSON.stringify(normalized(saved.value)));
const canSave = computed(() => isDirty.value && !isSaving.value && authState.status === 'signed-in' && !!validation.value.policy && domains.value !== null);

/** Fields of the chosen mode only, so switching modes and back doesn't count as an edit. */
function normalized(f: FromForm) {
    return {
        mode: f.mode,
        domain: f.mode === 'domain' ? f.domain : '',
        address: f.mode === 'address' ? f.address.trim() : '',
        onlyUnauthorized: f.mode === 'off' ? true : f.onlyUnauthorized
    };
}

function availabilityTag(port: number): { text: string; tone: string; title: string } {
    const connection = data.value?.connection;
    const state = portAvailability(connection?.observed_ports ?? [], connection?.observed_at, port);
    const when = connection?.observed_at ? `Last checked ${formatRelativeTime(connection.observed_at)}.` : 'Thinpost hasn\'t checked yet.';
    if (state === 'listening') return { text: 'Available', tone: 'success', title: when };
    if (state === 'not-listening') return { text: 'Not available right now', tone: 'warning', title: when };
    return { text: 'Not checked yet', tone: '', title: when };
}

let loadSeq = 0;

watch(appId, () => void load(), { immediate: true });
watch(
    () => form.value.mode,
    () => {
        touched.value = false;
        savedNote.value = null;
    }
);
watch(form, () => (savedNote.value = null), { deep: true });

async function load() {
    const id = appId.value;
    const seq = ++loadSeq;
    const isCurrent = () => seq === loadSeq;
    isLoading.value = true;
    loadError.value = null;
    await loadRuntimeFeatures();
    if (!isCurrent()) return;
    if (isUnavailable.value) {
        isLoading.value = false;
        return;
    }
    void loadDomains();
    try {
        const response = await DefaultApi.getAppSmtp({ path: { appId: id } });
        if (!isCurrent()) return;
        const parsed = parseAppSmtp(response.data);
        data.value = parsed;
        // Never overwrite edits in progress with a reload that only refreshed the credentials.
        if (!isDirty.value) resetFormTo(parsed);
    } catch (err) {
        if (!isCurrent()) return;
        loadError.value = describeApiError(err, { notFound: 'The app was not found.' });
    } finally {
        if (isCurrent()) isLoading.value = false;
    }
}

function resetFormTo(parsed: AppSmtp) {
    const next = formFromPolicy(parsed.settings.from);
    form.value = { ...next };
    saved.value = { ...next };
    touched.value = false;
}

function resetForm() {
    form.value = { ...saved.value };
    touched.value = false;
    saveError.value = null;
}

async function loadDomains() {
    const id = appId.value;
    domainsError.value = null;
    try {
        const response = await DefaultApi.listAppDomains({ path: { appId: id } });
        if (id !== appId.value) return;
        const list = response.data?.domains;
        if (!Array.isArray(list)) throw invalidResponse();
        domains.value = list.filter(domain => domain.active).map(domain => domain.name);
    } catch (err) {
        if (id !== appId.value) return;
        domains.value = null;
        domainsError.value = describeApiError(err, { notFound: 'The app was not found.' });
    }
}

function openIssue() {
    issueError.value = null;
    actionError.value = null;
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
        const response = await DefaultApi.createAppSmtpCredential({ path: { appId: id }, body: { name } });
        if (isUnmounted()) {
            showToast({ message: `Credential “${name}” was created after you left, so its password wasn't shown. Revoke it and create another.`, durationSecs: 10 });
            return;
        }
        isIssueOpen.value = false;
        const created = parseIssuedCredential(response.data);
        if (created) {
            issued.value = created;
        } else {
            actionError.value = {
                kind: 'failed',
                message: 'The credential was created, but Thinpost didn\'t return its password. Revoke the new credential if it is listed, then create another.'
            };
        }
        void load();
    } catch (err) {
        if (isUnmounted()) return;
        issueError.value = describeApiError(err, {
            mutation: true,
            notFound: 'The app was not found, or it is paused.',
            conflict: `This app already has ${MAX_ACTIVE_CREDENTIALS} active credentials. Revoke one you no longer use, then try again.`
        });
    } finally {
        isIssuing.value = false;
    }
}

async function revoke(credential: SmtpCredential) {
    const id = appId.value;
    if (revokingId.value) return;
    const ok = await showConfirmDestroy(
        'Revoke SMTP credential',
        `Revoke “${credential.name}”? Whatever still signs in with it will stop being able to send. This can't be undone.`
    );
    if (!ok) return;
    revokingId.value = credential.id;
    actionError.value = null;
    try {
        await DefaultApi.revokeAppSmtpCredential({ path: { appId: id, credentialId: credential.id } });
        showToast({ message: `Revoked “${credential.name}”`, durationSecs: 3 });
    } catch (err) {
        actionError.value = describeApiError(err, { mutation: true, notFound: 'The credential wasn\'t found, or it was already revoked.' });
    } finally {
        revokingId.value = null;
    }
    await load();
}

async function save() {
    touched.value = true;
    const policy = validation.value.policy;
    if (!policy || isSaving.value || !data.value) return;
    const id = appId.value;
    isSaving.value = true;
    saveError.value = null;
    savedNote.value = null;
    try {
        const response = await DefaultApi.updateAppSmtpSettings({ path: { appId: id }, body: { from: policy } });
        data.value = { ...data.value, settings: parseSmtpSettings(response.data) };
        resetFormTo(data.value);
        // The form watcher clears the note when the fields are replaced; show it once that has run.
        await nextTick();
        savedNote.value = 'Saved. It applies to mail sent from now on.';
    } catch (err) {
        saveError.value = describeApiError(err, {
            mutation: true,
            notFound: 'The app was not found.',
            conflict: 'That domain isn\'t a verified sending domain of this app. Check Sending domains, then try again.'
        });
    } finally {
        isSaving.value = false;
    }
}

onBeforeUnmount(() => {
    loadSeq++;
});
</script>

<style lang="scss" scoped>
.smtp-page {
    display: flex;
    flex-direction: column;
    gap: 16px;
}

.connect-body,
.smtp-body,
.from-body {
    display: flex;
    flex-direction: column;
    gap: 12px;
}

.ports {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
    gap: 16px;
    margin: 0;
    padding: 0;
    list-style: none;
}

.port-note {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
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

.modes {
    display: flex;
    flex-direction: column;
    gap: 8px;
    margin: 0;
    padding: 0;
    border: 0;
}

.mode,
.check {
    display: flex;
    align-items: flex-start;
    gap: 8px;

    > span {
        display: flex;
        flex-direction: column;
        gap: 2px;
    }
}

.field-group {
    display: flex;
    flex-direction: column;
    gap: 6px;
    max-width: 420px;

    label {
        color: var(--text);
        font-weight: 600;
    }

    .hint.invalid {
        color: var(--danger);
    }
}

.examples ul {
    margin: 4px 0 0;
    padding-left: 18px;
}

.saved {
    color: var(--success);
    font-size: 13px;
}

.form-actions {
    display: flex;
    gap: 8px;
}
</style>
