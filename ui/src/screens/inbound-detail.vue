<template>
    <div id="inbound-detail" class="detail-page">
        <Loader v-if="isLoading" label="Loading message" />

        <div v-else-if="isNotFound" class="card empty">
            <i class="fa-solid fa-inbox" aria-hidden="true" />
            <h2>Message not found</h2>
            <RouterLink class="btn" :to="{ name: 'app-inbound', params: { appId: context.appId.value } }">Back to inbound</RouterLink>
        </div>

        <ApiErrorNotice v-else-if="loadError" :error="loadError" :retry="load" />

        <template v-else-if="detail">
            <section class="card" aria-labelledby="inbound-heading">
                <div class="card-header">
                    <h2 id="inbound-heading" class="subject-title" data-fact="subject">
                        <span class="sr-only">Subject: </span>
                        <span v-if="row.enhancedProtection" class="muted">Protected. Shown with the message content.</span>
                        <span v-else-if="row.subject" :title="row.subject">{{ row.subject }}</span>
                        <span v-else class="muted">(no subject)</span>
                    </h2>
                    <span v-if="isExpired" class="tag" data-testid="expired-tag">Expired</span>
                    <div class="header-actions">
                        <button type="button" class="btn sm" :disabled="isRefreshing" @click="refresh">
                            <i class="fa-solid fa-rotate" :class="{ 'fa-spin': isRefreshing }" aria-hidden="true" />
                            {{ isRefreshing ? 'Refreshing…' : 'Refresh' }}
                        </button>
                        <button
                            type="button"
                            class="btn sm"
                            data-testid="download-eml"
                            :disabled="isExpired || isDownloading || authState.status !== 'signed-in'"
                            :title="isExpired ? 'The message is past its retention period and has been removed' : undefined"
                            @click="download"
                        >
                            <i class="fa-solid fa-download" aria-hidden="true" />
                            {{ isDownloading ? 'Downloading…' : 'Download .eml' }}
                        </button>
                    </div>
                </div>

                <div class="card-body">
                    <ApiErrorNotice v-if="refreshError" :error="refreshError" />
                    <ApiErrorNotice v-if="downloadError" :error="downloadError" />
                    <dl class="kv">
                        <div data-fact="from">
                            <dt>From</dt>
                            <dd v-if="row.from"><span class="v" :title="row.from">{{ row.from }}</span></dd>
                            <dd v-else class="muted">Not recorded</dd>
                        </div>
                        <div data-fact="recipients">
                            <dt title="The addresses the sender delivered it to">To</dt>
                            <dd>
                                <span class="v wrap">
                                    <span v-for="(address, i) in shownRecipients" :key="i" class="addr">{{ address }}<template v-if="i < shownRecipients.length - 1">,</template></span>
                                </span>
                                <button v-if="hiddenRecipients > 0" type="button" class="link-btn more" @click="showAllRecipients = true">+{{ hiddenRecipients }} more</button>
                            </dd>
                        </div>
                        <div>
                            <dt>Received</dt>
                            <dd><time class="v" :datetime="row.receivedAt">{{ formatDateTime(row.receivedAt) }}</time></dd>
                        </div>
                        <div data-fact="retention">
                            <dt>Content kept until</dt>
                            <dd>
                                <time class="v" :datetime="detail.contentExpiresAt">{{ formatDateTime(detail.contentExpiresAt) }}</time>
                                <span v-if="isExpired" class="tag">Expired (retention)</span>
                                <HelpButton topic="how long content is kept">
                                    <p>Thinpost keeps a received message for 30 days. After that its content is removed, so it can't be shown, downloaded or sent to your webhook again. The details on this page stay.</p>
                                </HelpButton>
                            </dd>
                        </div>
                        <div>
                            <dt>Message ID</dt>
                            <dd>
                                <code class="v id" :title="row.id">{{ row.id }}</code>
                                <button type="button" class="btn ghost sm icon" title="Copy message ID" @click="copyValue(row.id, 'message ID')">
                                    <i class="fa-solid fa-copy" aria-hidden="true" />
                                    <span class="sr-only">Copy message ID</span>
                                </button>
                            </dd>
                        </div>
                        <div data-fact="message-id-header">
                            <dt>Message-ID header</dt>
                            <dd v-if="detail.messageIdHeader">
                                <code class="v id" :title="detail.messageIdHeader">{{ detail.messageIdHeader }}</code>
                                <button type="button" class="btn ghost sm icon" title="Copy Message-ID header" @click="copyValue(detail.messageIdHeader, 'Message-ID header')">
                                    <i class="fa-solid fa-copy" aria-hidden="true" />
                                    <span class="sr-only">Copy Message-ID header</span>
                                </button>
                            </dd>
                            <dd v-else class="muted">Not recorded</dd>
                        </div>
                        <div>
                            <dt>Size</dt>
                            <dd><span class="v">{{ formatBytes(row.sizeBytes) }}</span></dd>
                        </div>
                        <div data-fact="enhanced-protection">
                            <dt>Enhanced Protection</dt>
                            <dd>
                                <span v-if="row.enhancedProtection" class="tag info"><i class="fa-solid fa-shield-halved" aria-hidden="true" />On</span>
                                <span v-else class="muted">Off</span>
                                <HelpButton topic="Enhanced Protection">
                                    <p>
                                        A reply to a message your app sent with Enhanced Protection is protected too. Its subject is hidden in lists, and viewing its content
                                        or downloading it is recorded in the audit log under your name.
                                    </p>
                                    <p>Your webhook always receives the whole message: it is your own mail. Apply any protection rules on your side.</p>
                                </HelpButton>
                            </dd>
                        </div>
                    </dl>
                </div>
            </section>

            <section class="card" aria-labelledby="arrived-heading">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="arrived-heading">How it arrived</h2>
                        <HelpButton topic="how it arrived">
                            <p>What the sending mail server told Thinpost when it handed the message over. The sender can type anything here, except the address of the server it connected from, which Thinpost saw itself.</p>
                        </HelpButton>
                    </div>
                </div>
                <div class="card-body">
                    <dl class="kv" data-testid="envelope">
                        <div>
                            <dt title="The address the sender gave as the return path">Sent by</dt>
                            <dd v-if="detail.envelope.mailFrom"><span class="v" :title="detail.envelope.mailFrom">{{ detail.envelope.mailFrom }}</span></dd>
                            <dd v-else class="muted">No return address (typical of automatic mail)</dd>
                        </div>
                        <div>
                            <dt>Server address</dt>
                            <dd v-if="detail.envelope.clientIp"><code class="v">{{ detail.envelope.clientIp }}</code></dd>
                            <dd v-else class="muted">Not recorded</dd>
                        </div>
                        <div>
                            <dt title="The name the sending server gave for itself (HELO)">Server name</dt>
                            <dd v-if="detail.envelope.helo"><span class="v" :title="detail.envelope.helo">{{ detail.envelope.helo }}</span></dd>
                            <dd v-else class="muted">Not recorded</dd>
                        </div>
                        <div>
                            <dt>Encryption</dt>
                            <dd v-if="detail.envelope.tls"><span class="v" :title="detail.envelope.tls">{{ detail.envelope.tls }}</span></dd>
                            <dd v-else class="muted">Not encrypted in transit</dd>
                        </div>
                    </dl>
                </div>
            </section>

            <section class="card" aria-labelledby="auth-heading">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="auth-heading">Sender checks</h2>
                        <HelpButton topic="sender checks">
                            <p>
                                Thinpost runs the usual sender checks and reports the results. It never turns mail away because of them: your app decides what a failure means.
                            </p>
                            <ul>
                                <li><strong>SPF</strong>: was the sending server allowed to send for the domain?</li>
                                <li><strong>DKIM</strong>: is the message signed by a domain, and does the signature hold?</li>
                                <li><strong>DMARC</strong>: do SPF or DKIM match the domain in the From line? This is the check that matters most for "is this really from them".</li>
                                <li><strong>ARC</strong>: forwarding trail. Thinpost notes that one is present but does not evaluate it.</li>
                            </ul>
                        </HelpButton>
                    </div>
                </div>
                <div class="table-wrap">
                    <table class="data compact" data-testid="authentication">
                        <caption class="sr-only">Sender check results</caption>
                        <thead>
                            <tr><th scope="col">Check</th><th scope="col">Result</th><th scope="col">Domain</th><th scope="col">Detail</th></tr>
                        </thead>
                        <tbody>
                            <tr data-check="spf">
                                <th scope="row">SPF</th>
                                <td><span class="tag" :class="authResult(auth.spf.result).tone" :title="authResult(auth.spf.result).description">{{ authResult(auth.spf.result).label }}</span></td>
                                <td>{{ auth.spf.domain ?? '—' }}</td>
                                <td class="muted">{{ spfDetail }}</td>
                            </tr>
                            <tr v-if="!auth.dkim.length" data-check="dkim">
                                <th scope="row">DKIM</th>
                                <td><span class="tag" :title="authResult('none').description">No signature</span></td>
                                <td>—</td>
                                <td class="muted" />
                            </tr>
                            <tr v-for="(signature, i) in auth.dkim" :key="i" data-check="dkim">
                                <th scope="row">DKIM<span v-if="auth.dkim.length > 1"> {{ i + 1 }}</span></th>
                                <td><span class="tag" :class="authResult(signature.result).tone" :title="authResult(signature.result).description">{{ authResult(signature.result).label }}</span></td>
                                <td>{{ signature.domain ?? '—' }}</td>
                                <td class="muted" />
                            </tr>
                            <tr data-check="dmarc">
                                <th scope="row">DMARC</th>
                                <td><span class="tag" :class="authResult(auth.dmarc.result).tone" :title="authResult(auth.dmarc.result).description">{{ authResult(auth.dmarc.result).label }}</span></td>
                                <td>{{ auth.dmarc.domain ?? '—' }}</td>
                                <td class="muted">{{ auth.dmarc.policy ? `The domain asks receivers to: ${auth.dmarc.policy}` : '' }}</td>
                            </tr>
                            <tr data-check="arc">
                                <th scope="row">ARC</th>
                                <td><span class="tag" title="Thinpost does not evaluate ARC chains.">Not evaluated</span></td>
                                <td>—</td>
                                <td class="muted">{{ auth.arcPresent ? 'The message carries ARC headers.' : '' }}</td>
                            </tr>
                        </tbody>
                    </table>
                </div>
            </section>

            <section class="card" aria-labelledby="reply-heading">
                <div class="card-header">
                    <div class="heading-with-help">
                        <h2 id="reply-heading">Reply to your message</h2>
                        <HelpButton topic="reply matching">
                            <p>
                                Thinpost looks at the In-Reply-To and References lines for the ID it put on a message your app sent. It is only a claim in the message: anyone
                                who saw that message could quote it.
                            </p>
                            <p>To trust a reply as coming from the person you wrote to, also check that its From address matches the recipient and that DMARC passed.</p>
                        </HelpButton>
                    </div>
                </div>
                <div class="card-body">
                    <p v-if="!detail.correlation.matched" class="muted" data-testid="no-correlation">This doesn't answer a message your app sent through Thinpost.</p>
                    <dl v-else class="kv" data-testid="correlation">
                        <div>
                            <dt>Your message</dt>
                            <dd>
                                <RouterLink :to="{ name: 'app-message', params: { appId: context.appId.value, messageId: detail.correlation.messageId! } }">
                                    <code>{{ detail.correlation.messageId }}</code>
                                </RouterLink>
                            </dd>
                        </div>
                        <div v-if="detail.correlation.deliveryId">
                            <dt title="The one recipient's copy of your message that this answers">Recipient copy</dt>
                            <dd><code class="v">{{ detail.correlation.deliveryId }}</code></dd>
                        </div>
                        <div v-else>
                            <dt>Recipient copy</dt>
                            <dd class="muted">Not known: your message went to several recipients and the sender didn't match just one</dd>
                        </div>
                        <div>
                            <dt>Found in</dt>
                            <dd>{{ detail.correlation.header === 'references' ? 'References line' : 'In-Reply-To line' }}</dd>
                        </div>
                        <div>
                            <dt title="Whether the From address is one of the people your message went to">From matches a recipient</dt>
                            <dd v-if="detail.correlation.fromMatchesRecipient === null" class="muted">Not checked</dd>
                            <dd v-else>
                                <span class="tag" :class="detail.correlation.fromMatchesRecipient ? 'success' : 'warning'">{{ detail.correlation.fromMatchesRecipient ? 'Yes' : 'No' }}</span>
                            </dd>
                        </div>
                    </dl>
                </div>
            </section>

            <section v-if="isExpired" class="card" aria-labelledby="gone-heading" data-testid="content-expired">
                <div class="card-header">
                    <h2 id="gone-heading">Content</h2>
                </div>
                <div class="card-body">
                    <div class="notice info">
                        <i class="fa-solid fa-hourglass-end" aria-hidden="true" />
                        <p class="notice-body">
                            <strong>Expired (retention).</strong> Thinpost keeps received messages for 30 days. This one has been removed, so its content can't be shown or
                            downloaded, and it can't be sent to a webhook again.
                        </p>
                    </div>
                </div>
            </section>
            <MessageContent
                v-else
                :key="row.id"
                :app-id="context.appId.value"
                :message-id="row.id"
                :enhanced-protection="row.enhancedProtection"
                :loader="loadContent"
                :errors="CONTENT_ERRORS"
                received
            />

            <WebhookDeliveryHistory
                :key="row.id"
                :app-id="context.appId.value"
                :app-active="app.active"
                :subscriptions="subscriptions"
                :inbound-id="row.id"
                :runtime-unavailable="webhooksUnavailable"
                @changed="loadSubscriptions"
            />
        </template>
    </div>
</template>

<script lang="ts" setup>
import { showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, shallowRef, watch } from 'vue';
import { useRoute } from 'vue-router';

import { apiErrorStatus, describeApiError, InvalidResponseError, type ApiErrorDescription, type DescribeOptions } from '@/api-errors';
import { useAppContext } from '@/app-context';
import { authState } from '@/auth';
import { confirmProtectedAccess, withAuditSuffix } from '@/enhanced-protection';
import { copyText, formatBytes, formatDateTime } from '@/format';
import { downloadInboundMessage } from '@/inbound-download';
import { authResult, readInboundDetail, type InboundDetailView } from '@/inbound-messages';
import { DefaultApi } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';
import MessageContent from '@/shared/components/message-content.vue';
import WebhookDeliveryHistory, { type SubscriptionSummary } from '@/shared/components/webhook-delivery-history.vue';
import { isOpaqueId } from '@/validation';

const EXPIRED = 'Expired (retention). Thinpost keeps received messages for 30 days and this one has been removed, so it can\'t be shown or downloaded.';
const CONTENT_ERRORS: DescribeOptions = {
    notFound: 'That message wasn\'t found.',
    conflict: EXPIRED,
    unavailable: 'The message couldn\'t be read right now. Try again in a moment.'
};
const DOWNLOAD_ERRORS: DescribeOptions = {
    notFound: 'That message wasn\'t found.',
    conflict: EXPIRED,
    unavailable: 'The message couldn\'t be downloaded right now. Try again in a moment.'
};
const RECIPIENTS_COLLAPSED = 10;

const route = useRoute();
const context = useAppContext();
const { app } = context;

const detail = shallowRef<InboundDetailView | null>(null);
const isLoading = ref(true);
const isNotFound = ref(false);
const loadError = ref<ApiErrorDescription | null>(null);
const isRefreshing = ref(false);
const refreshError = ref<ApiErrorDescription | null>(null);
const showAllRecipients = ref(false);
/** Set when a read answered 409 although the page didn't think the content had expired (clock skew, or removed early). */
const goneEarly = ref(false);

const isDownloading = ref(false);
const downloadError = ref<ApiErrorDescription | null>(null);
let revokePending: (() => void) | null = null;

const subscriptions = shallowRef<ReadonlyMap<string, SubscriptionSummary> | null>(null);
const webhooksUnavailable = ref(false);

const row = computed(() => detail.value!.row);
const auth = computed(() => detail.value!.authentication);
const isExpired = computed(() => !!detail.value && (detail.value.row.contentExpired || goneEarly.value));
const recipients = computed(() => detail.value?.envelope.recipients ?? []);
const shownRecipients = computed(() => (showAllRecipients.value ? recipients.value : recipients.value.slice(0, RECIPIENTS_COLLAPSED)));
const hiddenRecipients = computed(() => recipients.value.length - shownRecipients.value.length);
const spfDetail = computed(() => {
    const identity = auth.value.spf.identity;
    if (identity === 'mailfrom') return "Checked against the sender's return address.";
    if (identity === 'helo') return "Checked against the sending server's own name (the message had no return address).";
    return '';
});

let loadSeq = 0;
let isUnmounted = false;

watch([() => context.appId.value, () => route.params.inboundId], load, { immediate: true });
watch(() => context.appId.value, loadSubscriptions, { immediate: true });

onBeforeUnmount(() => {
    isUnmounted = true;
    revokePending?.();
});

async function fetchDetail(appId: string, inboundId: string): Promise<InboundDetailView> {
    const { data } = await DefaultApi.getInboundMessage({ path: { appId, inboundId } });
    return readInboundDetail(data);
}

async function load() {
    const appId = context.appId.value;
    const inboundId = route.params.inboundId;
    const seq = ++loadSeq;
    isLoading.value = true;
    isNotFound.value = false;
    loadError.value = null;
    refreshError.value = null;
    downloadError.value = null;
    isRefreshing.value = false;
    showAllRecipients.value = false;
    goneEarly.value = false;
    detail.value = null;
    if (!isOpaqueId(inboundId)) {
        isNotFound.value = true;
        isLoading.value = false;
        return;
    }
    try {
        const data = await fetchDetail(appId, inboundId);
        if (seq !== loadSeq || isUnmounted) return;
        detail.value = data;
    } catch (err) {
        if (seq !== loadSeq || isUnmounted) return;
        if (apiErrorStatus(err) === 404) isNotFound.value = true;
        else loadError.value = describeApiError(err, { unavailable: 'This message is unavailable right now. Try again in a moment.' });
    } finally {
        if (seq === loadSeq) isLoading.value = false;
    }
}

/** Reloads the details while keeping what is on screen; the webhook history has its own refresh. */
async function refresh() {
    const current = detail.value;
    if (!current || isRefreshing.value) return;
    const seq = loadSeq;
    isRefreshing.value = true;
    refreshError.value = null;
    try {
        const data = await fetchDetail(context.appId.value, current.row.id);
        if (seq !== loadSeq || isUnmounted) return;
        detail.value = data;
    } catch (err) {
        if (seq !== loadSeq || isUnmounted) return;
        const description = describeApiError(err, { notFound: 'That message wasn\'t found.' });
        refreshError.value = { ...description, message: `${description.message} The details below are from the last time they loaded.` };
    } finally {
        if (seq === loadSeq) isRefreshing.value = false;
    }
}

async function loadSubscriptions() {
    const appId = context.appId.value;
    try {
        const { data } = await DefaultApi.listAppWebhooks({ path: { appId } });
        if (isUnmounted || appId !== context.appId.value) return;
        const list: unknown = data?.webhooks;
        if (!Array.isArray(list)) throw new InvalidResponseError();
        subscriptions.value = new Map(
            list
                .filter(hook => isOpaqueId(hook?.id) && typeof hook.endpoint === 'string')
                .map(hook => [hook.id as string, { endpoint: hook.endpoint as string, status: hook.active ? 'ready' : 'disabled' } as SubscriptionSummary])
        );
        webhooksUnavailable.value = false;
    } catch (err) {
        if (isUnmounted) return;
        // Without the list the history still shows; resending is then blocked by the component itself.
        subscriptions.value = null;
        webhooksUnavailable.value = apiErrorStatus(err) === 503;
    }
}

/** The content loader for MessageContent: a 409 means the content is gone, which the whole page then shows. */
async function loadContent(): Promise<unknown> {
    const id = row.value.id;
    try {
        const { data } = await DefaultApi.getInboundMessageContent({ path: { appId: context.appId.value, inboundId: id } });
        return data;
    } catch (err) {
        if (apiErrorStatus(err) === 409) markExpired();
        throw err;
    }
}

function markExpired() {
    goneEarly.value = true;
}

async function download() {
    const current = detail.value;
    if (!current || isDownloading.value || authState.status !== 'signed-in') return;
    const confirmed = await confirmProtectedAccess('raw', { enhancedProtection: current.row.enhancedProtection });
    const seq = loadSeq;
    const isCurrent = () => !isUnmounted && seq === loadSeq && authState.status === 'signed-in';
    if (!confirmed || !isCurrent()) return;
    isDownloading.value = true;
    downloadError.value = null;
    try {
        await downloadInboundMessage({
            appId: context.appId.value,
            inboundId: current.row.id,
            shouldProceed: isCurrent,
            onUrl: revoke => {
                revokePending = revoke;
            }
        });
    } catch (err) {
        if (!isCurrent()) return;
        if (apiErrorStatus(err) === 409) markExpired();
        const description = describeApiError(err, DOWNLOAD_ERRORS);
        downloadError.value = current.row.enhancedProtection ? withAuditSuffix(description) : description;
        showToast({ message: 'Download failed', durationSecs: 3 });
    } finally {
        isDownloading.value = false;
    }
}

async function copyValue(value: string, what: string) {
    const ok = await copyText(value);
    showToast({ message: ok ? `Copied ${what}` : 'Couldn\'t copy', durationSecs: 2 });
}
</script>

<style lang="scss" scoped>
.detail-page {
    display: flex;
    flex-direction: column;
    gap: 16px;
}

.card-header {
    flex-wrap: wrap;
}

.subject-title {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
}

.header-actions {
    display: flex;
    gap: 8px;
}

.kv {
    display: grid;
    grid-template-columns: 170px minmax(0, 1fr);
    gap: 8px 16px;
    margin: 0;
    font-size: 13px;

    > div {
        display: contents;
    }

    dt {
        color: var(--text-3);
        white-space: nowrap;
    }

    dd {
        display: flex;
        align-items: center;
        gap: 6px;
        min-width: 0;
        margin: 0;
    }

    .v {
        min-width: 0;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;

        &.wrap {
            white-space: normal;
        }
    }

    .addr {
        display: inline-block;
        max-width: 100%;
        margin-right: 0.35em;
        overflow-wrap: anywhere;
    }

    .id {
        font-size: 12.5px;
    }

    .more {
        flex: none;
        white-space: nowrap;
    }

    .btn.icon.sm {
        flex: none;
        height: 22px;
    }
}

@media (max-width: 640px) {
    .kv {
        grid-template-columns: minmax(0, 1fr);
        gap: 0;

        dt {
            margin-top: 10px;
            font-size: 12px;
        }
    }
}
</style>
