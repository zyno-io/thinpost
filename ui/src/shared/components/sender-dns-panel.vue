<template>
    <article class="card dns" :aria-labelledby="`${id}-title`" :data-testid="`sender-dns-${domain.name}`">
        <div class="dns-header">
            <div class="heading-with-help">
                <h3 :id="`${id}-title`">SPF and DMARC for {{ domain.name }}</h3>
                <HelpButton topic="SPF and DMARC">
                    <p>
                        <strong>DMARC</strong> is a record on your own domain that tells receivers what to do with mail that fails its checks, and where to send reports. A good start is
                        <code>v=DMARC1; p=none; rua=mailto:you@example.com</code>: it changes nothing about delivery and sends you reports. Thinpost mail passes DMARC through DKIM, which signs
                        with your domain.
                    </p>
                    <p>
                        <strong>SPF</strong> is checked by receivers against the bounce address of a message, not against your domain's own SPF record, so you don't add Thinpost to it. By
                        default that address is one of Thinpost's names.
                    </p>
                    <p>A bounce domain makes SPF line up with your domain too. Add one in the panel above.</p>
                </HelpButton>
                <span v-if="view" class="tag" :class="view.summary.tone" data-testid="sender-dns-summary">{{ view.summary.text }}</span>
            </div>
            <button type="button" class="btn sm" data-testid="check-sender-dns" :disabled="!isSignedIn || isChecking" @click="check">
                {{ isChecking ? 'Checking…' : 'Check again' }}<span class="sr-only"> SPF and DMARC for {{ domain.name }}</span>
            </button>
        </div>

        <ApiErrorNotice v-if="error" :error="error" />

        <template v-if="view && result">
            <p class="result" data-testid="dmarc-result">
                <span class="tag" :class="view.dmarc.tone">{{ view.dmarc.label }}</span> <strong>DMARC:</strong> {{ view.dmarc.message }}
                <template v-if="result.dmarc.record"> Currently: <code>{{ result.dmarc.record }}</code></template>
            </p>
            <p class="result" data-testid="spf-result"><span class="tag" :class="view.spf.tone">{{ view.spf.label }}</span> <strong>SPF:</strong> {{ view.spf.message }}</p>
            <p class="hint">This check only reads DNS and never changes how mail is sent.</p>
        </template>
        <p v-else-if="!error" class="hint">Checking what receivers find for {{ domain.name }}…</p>
    </article>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, useId } from 'vue';

import { describeApiError, type ApiErrorDescription } from '@/api-errors';
import { authState } from '@/auth';
import { DefaultApi, type Domain, type SenderDomainDnsCheck } from '@/openapi-client-generated';
import { dmarcLine, parseSenderDnsCheck, senderDnsSummary, spfLine } from '@/sender-dns';

import ApiErrorNotice from './api-error-notice.vue';
import HelpButton from './help-button.vue';

const props = defineProps<{
    appId: string;
    /** A verified sending domain with a DKIM key. */
    domain: Domain;
}>();

const id = useId();
const result = shallowRef<SenderDomainDnsCheck | null>(null);
const error = ref<ApiErrorDescription | null>(null);
const isChecking = ref(false);
const isSignedIn = computed(() => authState.status === 'signed-in');
const view = computed(() => (result.value ? { summary: senderDnsSummary(result.value), dmarc: dmarcLine(result.value), spf: spfLine(result.value) } : null));

let seq = 0;
let isUnmounted = false;
onBeforeUnmount(() => {
    isUnmounted = true;
});
onMounted(() => void check());

async function check() {
    if (isChecking.value) return;
    const mine = ++seq;
    const name = props.domain.name;
    isChecking.value = true;
    error.value = null;
    try {
        const { data } = await DefaultApi.checkAppDomainDns({ path: { appId: props.appId, domain: name } });
        const parsed = parseSenderDnsCheck(data, name);
        if (isUnmounted || mine !== seq) return;
        result.value = parsed;
    } catch (err) {
        if (isUnmounted || mine !== seq) return;
        result.value = null;
        error.value = describeApiError(err, {
            notFound: `${name} wasn't found for this app. Reload to see where it stands.`,
            unavailable: "Thinpost couldn't look up DNS just now. Try again in a moment."
        });
    } finally {
        if (mine === seq) isChecking.value = false;
    }
}
</script>

<style lang="scss" scoped>
.dns {
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding: 16px 18px;
    box-shadow: none;
}

.dns-header {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 8px;

    h3 {
        font-size: 14px;
        font-weight: 600;
        overflow-wrap: anywhere;
    }
}

.result {
    overflow-wrap: anywhere;

    .tag {
        margin-right: 2px;
    }
}
</style>
