<template>
    <VfModal
        class="tp-modal"
        role="dialog"
        aria-modal="true"
        scrolls
        close-on-mask-click
        :aria-labelledby="`${id}-title`"
        @close="emit('close')"
        @form-submit="emit('close')"
    >
        <template #header>
            <span :id="`${id}-title`">
                Delivery headers
                <span v-if="recipientLabel" class="recipient-label">{{ recipientLabel }}</span>
            </span>
        </template>

        <div class="modal-body headers-body">
            <Loader v-if="isLoading" label="Loading headers" />
            <ApiErrorNotice v-else-if="error" :error="error" />
            <div v-else-if="discarded" class="notice warning" role="status">
                <i class="fa-solid fa-triangle-exclamation" aria-hidden="true" />
                <div class="notice-body"><p>Your session ended, so the headers were cleared.</p></div>
            </div>

            <template v-else-if="headers">
                <p v-if="recipientKind === 'bcc'" class="muted bcc-note" data-field="bcc-note">Bcc copy: sent as a blind copy; the headers list To and Cc only.</p>
                <p v-if="!headers.length" class="muted">No headers to show.</p>
                <pre v-else class="mono-block headers-raw" tabindex="0" aria-label="Delivery headers">{{ rawHeaders }}</pre>
            </template>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="!headers" @click="copyAll">Copy all</button>
            <button type="submit" class="btn primary">Close</button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { showToast, VfModal } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, useId, watch } from 'vue';

import { describeApiError, InvalidResponseError, type ApiErrorDescription } from '@/api-errors';
import { authState } from '@/auth';
import { withAuditSuffix } from '@/enhanced-protection';
import { copyText } from '@/format';
import { DefaultApi } from '@/openapi-client-generated';
import { useModalPresence } from '@/shared/modal-state';

import ApiErrorNotice from './api-error-notice.vue';
import Loader from './loader.vue';

interface HeaderLine {
    name: string;
    value: string;
}

const MAX_HEADERS = 100;

const props = defineProps<{
    appId: string;
    messageId: string;
    deliveryId: string;
    recipientLabel?: string;
    recipientKind?: 'to' | 'cc' | 'bcc' | null;
    enhancedProtection: boolean;
}>();

const emit = defineEmits<{ close: [] }>();

useModalPresence();
const id = useId();

// Header values can contain the subject and recipient, so they live only in memory and are dropped on close or session end.
const headers = shallowRef<HeaderLine[] | null>(null);
const isLoading = ref(true);
const error = ref<ApiErrorDescription | null>(null);
const discarded = ref(false);
let isUnmounted = false;

// Raw header block exactly as sent; RFC 2047 encoded words are not decoded.
const rawHeaders = computed(() => headers.value?.map(h => `${h.name}: ${h.value}`).join('\n') ?? '');

onMounted(() => void load());
onBeforeUnmount(() => {
    isUnmounted = true;
    headers.value = null;
});
watch(
    () => authState.status,
    status => {
        if (status !== 'signed-in' && (headers.value || isLoading.value)) {
            headers.value = null;
            error.value = null;
            isLoading.value = false;
            discarded.value = true;
        }
    }
);

function readHeaders(raw: unknown): HeaderLine[] | null {
    if (!Array.isArray(raw) || raw.length > MAX_HEADERS) return null;
    const lines: HeaderLine[] = [];
    for (const item of raw) {
        if (!item || typeof item !== 'object') return null;
        const v = item as Record<string, unknown>;
        if (typeof v.name !== 'string' || typeof v.value !== 'string' || (v.decoded_value !== undefined && typeof v.decoded_value !== 'string')) return null;
        lines.push({ name: v.name, value: v.value });
    }
    return lines;
}

async function load() {
    try {
        const response = await DefaultApi.getAppDeliveryHeaders({
            path: { appId: props.appId, messageId: props.messageId, deliveryId: props.deliveryId }
        });
        const data = response.data;
        if (isUnmounted || authState.status !== 'signed-in') return;
        const parsed = readHeaders(data?.headers);
        if (!parsed) throw new InvalidResponseError();
        headers.value = parsed;
    } catch (err) {
        if (isUnmounted || authState.status !== 'signed-in') return;
        const description = describeApiError(err, {
            notFound: 'This delivery wasn\'t found, or its message hasn\'t been prepared yet.',
            unavailable: 'The message couldn\'t be read right now. Try again in a moment.'
        });
        error.value = props.enhancedProtection ? withAuditSuffix(description) : description;
    } finally {
        if (!isUnmounted) isLoading.value = false;
    }
}

async function copyAll() {
    const lines = headers.value;
    if (!lines) return;
    const ok = await copyText(lines.map(h => `${h.name}: ${h.value}`).join('\r\n'));
    showToast({ message: ok ? 'Copied headers' : 'Couldn\'t copy', durationSecs: 2 });
}
</script>

<style lang="scss" scoped>
.recipient-label {
    margin-left: 8px;
    color: var(--text-3);
    font-size: 12px;
    font-weight: 400;
}

.bcc-note {
    margin: 0 0 8px;
}

.headers-body {
    width: min(720px, calc(100vw - 72px));
}

// The modal scrolls vertically; long header lines scroll sideways instead of wrapping.
.headers-raw {
    max-height: none;
    margin: 0;
    overflow-x: auto;
    line-height: 1.5;
    white-space: pre;
    overflow-wrap: normal;
}
</style>
