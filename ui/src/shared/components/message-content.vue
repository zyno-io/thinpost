<template>
    <section class="card message-content" aria-labelledby="content-heading">
        <div class="card-header">
            <div class="heading-with-help">
                <h2 id="content-heading">
                    Content
                    <span v-if="enhancedProtection" class="tag info"><i class="fa-solid fa-shield-halved" aria-hidden="true" />Enhanced Protection</span>
                </h2>
                <HelpButton topic="message content">
                    <p>Images and other content from the internet are never loaded here. HTML is shown in an isolated preview.</p>
                    <p>Treat the content as untrusted. It stays on this page only and is cleared when you hide it or leave.</p>
                    <p v-if="enhancedProtection">
                        This message uses Enhanced Protection. Its addresses, subject, contents and attachments load only when you ask. Viewing them,
                        including inline images, is recorded in the audit log under your name.
                    </p>
                </HelpButton>
            </div>
            <div v-if="body" class="header-actions">
                <button
                    v-if="body.raw"
                    type="button"
                    class="btn sm"
                    data-testid="download-original"
                    :disabled="isDownloadingRaw || authState.status !== 'signed-in'"
                    @click="downloadOriginal"
                >
                    <i class="fa-solid fa-download" aria-hidden="true" />
                    {{ isDownloadingRaw ? 'Downloading…' : 'Download original (.eml)' }}
                </button>
                <button type="button" class="btn sm" @click="hide">
                    <i class="fa-solid fa-eye-slash" aria-hidden="true" />
                    Hide content
                </button>
            </div>
            <button v-else type="button" class="btn sm" :disabled="isLoading || authState.status !== 'signed-in'" @click="show">
                <i class="fa-solid fa-file-lines" aria-hidden="true" />
                {{ isLoading ? 'Loading…' : 'Show content' }}
            </button>
        </div>

        <div v-if="isLoading || error || body" class="card-body content-body">
            <Loader v-if="isLoading" label="Loading content" />

            <ApiErrorNotice v-if="error" :error="error" />

            <template v-if="body">
                <p class="hint untrusted-note">
                    {{ received ? 'This is the message as it arrived.' : 'This is the content your app sent.' }} Scripts, links and content from the internet are turned
                    off.
                </p>
                <ApiErrorNotice v-if="rawError" :error="rawError" />
                <p v-if="body.raw" class="hint raw-note" data-testid="raw-note">
                    This message was sent as a complete email message. Attachments are part of it, so download the original (.eml) to get them.
                </p>
                <p v-if="body.omitted.length" class="hint omitted-note" data-testid="omitted-note">
                    Part of this message is too large or in a form Thinpost can't show here ({{ omittedLabel }}). Download the .eml to see everything.
                </p>
                <dl class="envelope">
                    <div>
                        <dt>From</dt>
                        <dd :title="body.from">{{ body.from }}</dd>
                    </div>
                    <div v-if="body.replyTo.length">
                        <dt>Reply-To</dt>
                        <dd :title="body.replyTo.join(', ')">{{ body.replyTo.join(', ') }}</dd>
                    </div>
                    <div>
                        <dt>To</dt>
                        <dd v-if="body.to.length" :title="body.to.join(', ')">{{ body.to.join(', ') }}</dd>
                        <dd v-else class="muted">None</dd>
                    </div>
                    <div v-if="body.cc.length">
                        <dt>Cc</dt>
                        <dd :title="body.cc.join(', ')">{{ body.cc.join(', ') }}</dd>
                    </div>
                    <div v-if="body.bcc.length">
                        <dt>Bcc</dt>
                        <dd :title="body.bcc.join(', ')">{{ body.bcc.join(', ') }}</dd>
                    </div>
                    <div>
                        <dt>Subject</dt>
                        <dd :title="body.subject">{{ body.subject }}</dd>
                    </div>
                </dl>

                <div class="tabs" role="tablist" aria-label="Content views" @keydown="onTabKeydown">
                    <button class="tab"
                        v-for="item in TABS"
                        :id="`${uid}-tab-${item.id}`"
                        :key="item.id"
                        type="button"
                        role="tab"
                        :aria-selected="tab === item.id"
                        :aria-controls="`${uid}-panel-${item.id}`"
                        :tabindex="tab === item.id ? 0 : -1"
                        :disabled="item.id === 'rendered' && !body.html"
                        @click="tab = item.id"
                    >
                        {{ item.label }}
                    </button>
                </div>

                <div v-if="tab === 'rendered'" :id="`${uid}-panel-rendered`" role="tabpanel" :aria-labelledby="`${uid}-tab-rendered`" class="panel">
                    <p v-if="inlinePending" class="hint">Loading inline images…</p>
                    <template v-else-if="preview">
                        <p v-if="preview.ok && (preview.blockedResources > 0 || preview.unresolvedInline > 0)" class="hint blocked-note">
                            <i class="fa-solid fa-shield-halved" aria-hidden="true" />
                            <span v-if="preview.blockedResources > 0">
                                {{ preview.blockedResources }} external {{ preview.blockedResources === 1 ? 'item' : 'items' }} blocked.
                            </span>
                            <span v-if="preview.unresolvedInline > 0">
                                {{ preview.unresolvedInline }} inline {{ preview.unresolvedInline === 1 ? 'image' : 'images' }} not shown.
                            </span>
                        </p>
                        <div v-if="!preview.ok && preview.reason === 'too-large'" class="notice info">
                            <i class="fa-solid fa-circle-info" aria-hidden="true" />
                            <div class="notice-body"><p>This HTML is too large to preview. Use HTML source instead.</p></div>
                        </div>
                        <div v-else-if="!preview.ok" class="notice warning">
                            <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                            <div class="notice-body"><p>This HTML can't be previewed safely. Use HTML source instead.</p></div>
                        </div>
                        <div v-else class="preview-frame">
                            <iframe sandbox="" referrerpolicy="no-referrer" title="HTML preview (isolated)" :srcdoc="preview.srcdoc" />
                        </div>
                    </template>
                </div>
                <div v-else-if="tab === 'text'" :id="`${uid}-panel-text`" role="tabpanel" :aria-labelledby="`${uid}-tab-text`" class="panel">
                    <pre v-if="body.text" class="mono-block" tabindex="0" aria-label="Text part">{{ body.text }}</pre>
                    <p v-else class="muted">No text part.</p>
                </div>
                <div v-else :id="`${uid}-panel-source`" role="tabpanel" :aria-labelledby="`${uid}-tab-source`" class="panel">
                    <pre v-if="body.html" class="mono-block" tabindex="0" aria-label="HTML part source">{{ body.html }}</pre>
                    <p v-else class="muted">No HTML part.</p>
                </div>

                <section v-if="body.attachments.length" class="attachments-section" aria-labelledby="attachments-heading">
                    <h3 id="attachments-heading">Attachments ({{ body.attachments.length }})</h3>
                    <ApiErrorNotice v-if="downloadError" :error="downloadError" />
                    <ul class="attachments">
                        <li v-for="attachment in body.attachments" :key="attachment.index">
                            <i class="fa-solid" :class="attachmentIcon(attachment.contentType)" aria-hidden="true" />
                            <span class="attachment-name">
                                <template v-if="attachment.filename">{{ attachment.filename }}</template>
                                <span v-else class="muted">Unnamed attachment</span>
                            </span>
                            <code class="attachment-type">{{ attachment.contentType }}</code>
                            <span class="muted">{{ formatBytes(attachment.size) }}</span>
                            <span v-if="attachment.inline" class="tag">Inline</span>
                            <button class="btn sm"
                                type="button"
                                :disabled="downloading.has(attachment.index) || authState.status !== 'signed-in'"
                                @click="download(attachment)"
                            >
                                <i class="fa-solid fa-download" aria-hidden="true" />
                                {{ downloading.has(attachment.index) ? 'Downloading…' : 'Download' }}<span class="sr-only"> {{ attachment.filename }}</span>
                            </button>
                        </li>
                    </ul>
                </section>
            </template>
        </div>
    </section>
</template>

<script lang="ts" setup>
import { showToast } from '@zyno-io/vue-foundation';
import { computed, nextTick, onBeforeUnmount, onMounted, ref, shallowRef, useId, watch } from 'vue';

import { describeApiError, InvalidResponseError, type ApiErrorDescription, type DescribeOptions } from '@/api-errors';
import { downloadAttachment } from '@/attachment-download';
import { downloadMessageRaw } from '@/message-raw-download';
import { authState } from '@/auth';
import { confirmProtectedAccess, withAuditSuffix } from '@/enhanced-protection';
import { formatBytes } from '@/format';
import { readMessageBody, type AttachmentInfo, type ParsedMessageBody } from '@/message-body';
import { buildPreviewDocument, normalizeContentId, referencedContentIds } from '@/message-preview';
import { DefaultApi } from '@/openapi-client-generated';
import ApiErrorNotice from '@/shared/components/api-error-notice.vue';
import HelpButton from '@/shared/components/help-button.vue';
import Loader from '@/shared/components/loader.vue';

const props = defineProps<{
    appId: string;
    messageId: string;
    enhancedProtection: boolean;
    /**
     * Loads the body response (the data of a MessageBody). Without it the outbound message's body is read.
     * Whatever it returns is validated here, like any response.
     */
    loader?: () => Promise<unknown>;
    /** The message arrived from outside (inbound mail) rather than being sent by the app; changes the wording. */
    received?: boolean;
    /** Replaces the default error wording of a failed load (for example the 409 of expired content). */
    errors?: DescribeOptions;
}>();

type TabId = 'rendered' | 'text' | 'source';
const TABS: Array<{ id: TabId; label: string }> = [
    { id: 'rendered', label: 'Rendered' },
    { id: 'text', label: 'Text' },
    { id: 'source', label: 'HTML source' }
];

const INLINE_IMAGE_TYPES = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp', 'image/bmp', 'image/avif']);
const MAX_INLINE_IMAGE_BYTES = 5 * 1024 * 1024;
const MAX_INLINE_IMAGES = 20;
const MAX_INLINE_TOTAL_BYTES = 15 * 1024 * 1024;
const INLINE_CONCURRENCY = 3;
const OBJECT_URL_TTL_MS = 30_000;

const BODY_ERRORS = {
    notFound: 'That message wasn\'t found.',
    unavailable: 'The message couldn\'t be read right now. Try again in a moment.'
};
const ATTACHMENT_ERRORS = {
    notFound: 'That attachment wasn\'t found.',
    unavailable: 'The attachment couldn\'t be read right now. Try again in a moment.'
};

const RAW_ERRORS = {
    notFound: 'The original message wasn\'t found.',
    unavailable: 'The original message couldn\'t be read right now. Try again in a moment.'
};

const uid = useId();

// Message contents live only in this component's memory and are dropped on hide, unmount or session end.
const body = shallowRef<ParsedMessageBody | null>(null);
const isLoading = ref(false);
const error = ref<ApiErrorDescription | null>(null);
const tab = ref<TabId>('rendered');
const inlineImages = shallowRef<ReadonlyMap<string, string>>(new Map());
const inlinePending = ref(false);
const downloading = ref(new Set<number>());
const downloadError = ref<ApiErrorDescription | null>(null);
const isDownloadingRaw = ref(false);
const rawError = ref<ApiErrorDescription | null>(null);

let bodySeq = 0;
let isUnmounted = false;
const objectUrls = new Map<string, ReturnType<typeof setTimeout>>();

const omittedLabel = computed(() => (body.value?.omitted ?? []).map(part => (part === 'html' ? 'the HTML' : part === 'text' ? 'the text' : 'the headers')).join(' and '));

const preview = computed(() => {
    const current = body.value;
    if (!current || !current.html) return null;
    return buildPreviewDocument(current.html, { inlineImages: inlineImages.value });
});

onMounted(() => {
    if (!props.enhancedProtection && authState.status === 'signed-in') void show();
});

watch(
    () => authState.status,
    status => {
        if (status !== 'signed-in') hide();
    }
);

onBeforeUnmount(() => {
    // The confirm dialog is global and can outlive this component; this stops a later confirmation from fetching.
    isUnmounted = true;
    hide();
    revokeAllObjectUrls();
});

async function show() {
    if (isLoading.value || authState.status !== 'signed-in') return;
    if (props.enhancedProtection) {
        const confirmed = await confirmProtectedAccess('content', { enhancedProtection: true });
        if (!confirmed || isUnmounted || authState.status !== 'signed-in' || isLoading.value) return;
    }
    const thisLoad = ++bodySeq;
    const isCurrent = () => !isUnmounted && thisLoad === bodySeq && authState.status === 'signed-in';
    isLoading.value = true;
    error.value = null;
    try {
        const data = props.loader
            ? await props.loader()
            : (await DefaultApi.getAppMessageBody({ path: { appId: props.appId, messageId: props.messageId } })).data;
        if (!isCurrent()) return;
        const parsed = readMessageBody(data);
        if (!parsed) throw new InvalidResponseError();
        body.value = parsed;
        tab.value = parsed.html ? 'rendered' : 'text';
        inlinePending.value = referencedContentIds(parsed.html).length > 0;
        void loadInlineImages(parsed, thisLoad, isCurrent);
    } catch (err) {
        if (!isCurrent()) return;
        const description = describeApiError(err, { ...BODY_ERRORS, ...props.errors });
        error.value = props.enhancedProtection ? withAuditSuffix(description) : description;
    } finally {
        if (thisLoad === bodySeq) isLoading.value = false;
    }
}

function hide() {
    bodySeq++;
    body.value = null;
    error.value = null;
    isLoading.value = false;
    inlineImages.value = new Map();
    inlinePending.value = false;
    downloadError.value = null;
    rawError.value = null;
    isDownloadingRaw.value = false;
    downloading.value = new Set();
    tab.value = 'rendered';
    revokeAllObjectUrls();
}

// ---- Inline images ----

function bytesToBase64(bytes: Uint8Array): string {
    let binary = '';
    const chunk = 0x8000;
    for (let i = 0; i < bytes.length; i += chunk) binary += String.fromCharCode(...bytes.subarray(i, i + chunk));
    return btoa(binary);
}

function baseMime(contentType: string): string {
    return (contentType.split(';')[0] ?? '').trim().toLowerCase();
}

function pickInlineCandidates(parsed: ParsedMessageBody): Array<{ cid: string; attachment: AttachmentInfo }> {
    const referenced = new Set(referencedContentIds(parsed.html));
    const picked: Array<{ cid: string; attachment: AttachmentInfo }> = [];
    const seen = new Set<string>();
    let total = 0;
    for (const attachment of parsed.attachments) {
        if (!attachment.contentId || picked.length >= MAX_INLINE_IMAGES) continue;
        const cid = normalizeContentId(attachment.contentId);
        if (!referenced.has(cid) || seen.has(cid)) continue;
        if (!INLINE_IMAGE_TYPES.has(baseMime(attachment.contentType)) || attachment.size > MAX_INLINE_IMAGE_BYTES) continue;
        if (total + attachment.size > MAX_INLINE_TOTAL_BYTES) continue;
        total += attachment.size;
        seen.add(cid);
        picked.push({ cid, attachment });
    }
    return picked;
}

async function loadInlineImages(parsed: ParsedMessageBody, thisLoad: number, isCurrent: () => boolean) {
    const candidates = pickInlineCandidates(parsed);
    const resolved = new Map<string, string>();
    let cursor = 0;
    async function worker() {
        while (cursor < candidates.length) {
            const candidate = candidates[cursor++]!;
            try {
                const response = await DefaultApi.downloadAppMessageAttachment({
                    path: { appId: props.appId, messageId: props.messageId, index: candidate.attachment.index },
                    parseAs: 'blob'
                });
                const blob = response.data;
                if (!isCurrent()) return;
                if (!(blob instanceof Blob)) continue;
                const buffer = await blob.arrayBuffer();
                if (!isCurrent()) return;
                // The server always sends application/octet-stream, so the declared type is used.
                resolved.set(candidate.cid, `data:${baseMime(candidate.attachment.contentType)};base64,${bytesToBase64(new Uint8Array(buffer))}`);
            } catch {
                // A failed inline image counts as unresolved.
            }
        }
    }
    await Promise.all(Array.from({ length: Math.min(INLINE_CONCURRENCY, candidates.length) }, worker));
    if (!isCurrent() || thisLoad !== bodySeq) return;
    inlineImages.value = resolved;
    inlinePending.value = false;
}

// ---- Attachments ----

function attachmentIcon(contentType: string): string {
    const type = baseMime(contentType);
    if (type.startsWith('image/')) return 'fa-file-image';
    if (type === 'application/pdf') return 'fa-file-pdf';
    if (type.startsWith('text/')) return 'fa-file-lines';
    if (type.includes('zip') || type.includes('compressed') || type.includes('tar')) return 'fa-file-zipper';
    return 'fa-file';
}

function registerObjectUrl(url: string) {
    const timer = setTimeout(() => {
        URL.revokeObjectURL(url);
        objectUrls.delete(url);
    }, OBJECT_URL_TTL_MS);
    objectUrls.set(url, timer);
}

function revokeAllObjectUrls() {
    for (const [url, timer] of objectUrls) {
        clearTimeout(timer);
        URL.revokeObjectURL(url);
    }
    objectUrls.clear();
}

async function download(attachment: AttachmentInfo) {
    if (downloading.value.has(attachment.index) || authState.status !== 'signed-in') return;
    const confirmed = await confirmProtectedAccess('attachment', { enhancedProtection: props.enhancedProtection });
    const seq = bodySeq;
    const isCurrent = () => !isUnmounted && seq === bodySeq && authState.status === 'signed-in';
    if (!confirmed || !isCurrent()) return;
    downloading.value = new Set(downloading.value).add(attachment.index);
    downloadError.value = null;
    try {
        await downloadAttachment({
            appId: props.appId,
            messageId: props.messageId,
            attachment,
            register: registerObjectUrl,
            shouldProceed: isCurrent
        });
    } catch (err) {
        if (!isCurrent()) return;
        const description = describeApiError(err, ATTACHMENT_ERRORS);
        downloadError.value = props.enhancedProtection ? withAuditSuffix(description) : description;
        showToast({ message: 'Download failed', durationSecs: 3 });
    } finally {
        if (seq === bodySeq) {
            const next = new Set(downloading.value);
            next.delete(attachment.index);
            downloading.value = next;
        }
    }
}

async function downloadOriginal() {
    if (isDownloadingRaw.value || authState.status !== 'signed-in') return;
    const confirmed = await confirmProtectedAccess('raw', { enhancedProtection: props.enhancedProtection });
    const seq = bodySeq;
    const isCurrent = () => !isUnmounted && seq === bodySeq && authState.status === 'signed-in';
    if (!confirmed || !isCurrent()) return;
    isDownloadingRaw.value = true;
    rawError.value = null;
    try {
        await downloadMessageRaw({ appId: props.appId, messageId: props.messageId, register: registerObjectUrl, shouldProceed: isCurrent });
    } catch (err) {
        if (!isCurrent()) return;
        const description = describeApiError(err, RAW_ERRORS);
        rawError.value = props.enhancedProtection ? withAuditSuffix(description) : description;
        showToast({ message: 'Download failed', durationSecs: 3 });
    } finally {
        if (seq === bodySeq) isDownloadingRaw.value = false;
    }
}

// ---- Tabs ----

function onTabKeydown(event: KeyboardEvent) {
    const keys = ['ArrowLeft', 'ArrowRight', 'Home', 'End'];
    if (!keys.includes(event.key) || !body.value) return;
    const enabled = TABS.filter(item => item.id !== 'rendered' || body.value!.html);
    const current = enabled.findIndex(item => item.id === tab.value);
    let next = current;
    if (event.key === 'ArrowRight') next = (current + 1) % enabled.length;
    else if (event.key === 'ArrowLeft') next = (current - 1 + enabled.length) % enabled.length;
    else if (event.key === 'Home') next = 0;
    else next = enabled.length - 1;
    event.preventDefault();
    const target = enabled[next]!;
    tab.value = target.id;
    void nextTick(() => document.getElementById(`${uid}-tab-${target.id}`)?.focus());
}
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

.header-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
}

.content-body {
    display: flex;
    flex-direction: column;
    gap: 14px;
}

.envelope {
    display: grid;
    grid-template-columns: 140px minmax(0, 1fr);
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
        min-width: 0;
        margin: 0;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
    }
}

@media (max-width: 640px) {
    .envelope {
        grid-template-columns: minmax(0, 1fr);
        gap: 0;

        dt {
            margin-top: 10px;
            font-size: 12px;
        }

        dd {
            white-space: normal;
            overflow-wrap: anywhere;
        }
    }
}

.panel {
    display: flex;
    flex-direction: column;
    gap: 10px;
}

.blocked-note {
    display: flex;
    flex-wrap: wrap;
    gap: 4px 8px;
}

.preview-frame {
    height: min(70vh, 720px);
    min-height: 360px;
    overflow: hidden;
    resize: vertical;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: #fff;

    iframe {
        width: 100%;
        height: 100%;
        border: 0;
        color-scheme: light;
    }
}

.attachments-section {
    display: flex;
    flex-direction: column;
    gap: 8px;

    h3 {
        font-size: 13px;
        font-weight: 600;
    }
}

.attachments {
    display: flex;
    flex-direction: column;
    margin: 0;
    padding: 0;
    list-style: none;
    border: 1px solid var(--border);
    border-radius: var(--radius);

    li {
        display: flex;
        align-items: center;
        flex-wrap: wrap;
        gap: 6px 12px;
        padding: 8px 12px;
    }

    li + li {
        border-top: 1px solid var(--border);
    }

    .attachment-name {
        flex: 1;
        min-width: 120px;
        overflow-wrap: anywhere;
    }

    .attachment-type {
        color: var(--text-3);
        font-size: 12px;
    }
}
</style>
