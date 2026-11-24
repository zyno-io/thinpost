<template>
    <article class="card dns" :aria-labelledby="`${id}-title`" :data-testid="`sender-dns-${domain.name}`">
        <div class="dns-header">
            <div class="heading-with-help">
                <h3 :id="`${id}-title`">SPF and DMARC for {{ domain.name }}</h3>
                <HelpButton topic="SPF and DMARC">
                    <p>
                        <strong>DMARC</strong> is a TXT record on your domain. Thinpost mail passes it through DKIM, which signs with your domain, so <code>p=none</code> is fine for delivery.
                    </p>
                    <p><strong>SPF</strong> is checked on the bounce address, not on your domain's own SPF record, so Thinpost never needs an <code>include:</code> there.</p>
                </HelpButton>
                <span v-if="view" class="tag" :class="view.summary.tone" data-testid="sender-dns-summary">{{ view.summary.label }}</span>
            </div>
            <button type="button" class="btn sm" data-testid="check-sender-dns" :disabled="!isSignedIn || isChecking" @click="check">
                {{ isChecking ? 'Checking…' : 'Check again' }}<span class="sr-only"> SPF and DMARC for {{ domain.name }}</span>
            </button>
        </div>

        <ApiErrorNotice v-if="error" :error="error" />

        <div v-if="view" class="records">
            <section class="rec" data-testid="dmarc-result">
                <h4 class="label">DMARC</h4>
                <div class="body">
                    <p class="line">
                        <span class="tag" :class="view.dmarc.tag.tone" data-testid="dmarc-tag">{{ view.dmarc.tag.label }}</span>
                        <code v-if="view.dmarc.location" class="loc" data-testid="dmarc-location">{{ view.dmarc.location }}</code>
                    </p>
                    <p v-if="view.dmarc.segments.length" class="record mono" data-testid="dmarc-record">
                        <template v-for="(part, i) in view.dmarc.segments" :key="i"
                            ><span class="part" :class="part.state" :data-state="part.state" :title="part.hint"
                                >{{ part.text }}<span v-if="part.hint" class="sr-only"> ({{ part.hint }})</span></span
                            ><template v-if="i < view.dmarc.segments.length - 1">; </template></template
                        ><span v-if="view.dmarc.truncated" class="muted" data-testid="dmarc-truncated"> …</span>
                    </p>
                    <ul v-if="view.dmarc.records.length" class="record-list" data-testid="dmarc-records">
                        <li v-for="(record, i) in view.dmarc.records" :key="i" class="record mono"><span class="part problem" data-state="problem">{{ record }}</span></li>
                    </ul>
                    <div v-if="view.dmarc.fix" class="fix" data-testid="dmarc-fix">
                        <template v-if="view.dmarc.fix.kind === 'keep-one'">
                            <span class="fix-text">{{ view.dmarc.fix.text }}</span>
                        </template>
                        <template v-else>
                            <span class="fix-label">{{ view.dmarc.fix.kind === 'add' ? 'Add:' : 'Update:' }}</span>
                            <span class="fix-value"
                                ><span v-if="view.dmarc.fix.kind === 'add'" class="fix-name mono">{{ view.dmarc.fix.name }}</span
                                ><code data-testid="dmarc-fix-value">{{ view.dmarc.fix.value }}</code></span
                            >
                            <button type="button" class="btn sm" data-testid="dmarc-fix-copy" @click="copyFix(view.dmarc.fix.value)">
                                <i class="fa-solid fa-copy" aria-hidden="true" />
                                Copy<span class="sr-only"> DMARC record</span>
                            </button>
                            <span v-if="view.dmarc.fix.kind === 'update' && view.dmarc.fix.warning" class="fix-warning" data-testid="dmarc-fix-warning">{{ view.dmarc.fix.warning }}</span>
                        </template>
                    </div>
                </div>
            </section>
            <section class="rec" data-testid="spf-result">
                <h4 class="label">SPF</h4>
                <div class="body">
                    <p class="line">
                        <span class="tag" :class="view.spf.result.tone" data-testid="spf-tag">{{ view.spf.result.label }}</span>
                        <span v-if="view.spf.alignment" class="tag" :class="view.spf.alignment.tone" :title="view.spf.alignment.title" data-testid="spf-alignment">{{
                            view.spf.alignment.label
                        }}<span v-if="view.spf.alignment.title" class="sr-only"> ({{ view.spf.alignment.title }})</span></span>
                        <code v-if="view.spf.envelope" class="loc" data-testid="spf-envelope">{{ view.spf.envelope }}</code>
                        <span v-if="view.spf.standard" class="muted">(Thinpost)</span>
                    </p>
                    <p v-if="view.spf.note" class="muted" data-testid="spf-note">{{ view.spf.note }}</p>
                </div>
            </section>
        </div>
        <p v-else-if="!error" class="hint">Checking what receivers find for {{ domain.name }}…</p>
    </article>
</template>

<script setup lang="ts">
import { showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, useId } from 'vue';

import { describeApiError, type ApiErrorDescription } from '@/api-errors';
import { authState } from '@/auth';
import { copyText } from '@/format';
import { DefaultApi, type Domain, type SenderDomainDnsCheck } from '@/openapi-client-generated';
import { dmarcView, parseSenderDnsCheck, senderDnsSummary, spfView } from '@/sender-dns';

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
const view = computed(() => (result.value ? { summary: senderDnsSummary(result.value), dmarc: dmarcView(result.value), spf: spfView(result.value) } : null));

async function copyFix(value: string) {
    const ok = await copyText(value);
    showToast({ message: ok ? 'Copied DMARC record' : "Couldn't copy", durationSecs: 2 });
}

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

.records {
    display: flex;
    flex-direction: column;
}

.rec {
    display: grid;
    grid-template-columns: 56px minmax(0, 1fr);
    gap: 4px 12px;
    padding: 10px 0;
    border-top: 1px solid var(--border);

    &:last-child {
        padding-bottom: 0;
    }

    @media (max-width: 600px) {
        grid-template-columns: minmax(0, 1fr);
    }
}

.label {
    color: var(--text-3);
    font-size: 11px;
    font-weight: 600;
    letter-spacing: 0.5px;
    line-height: 19px;
}

.body {
    display: flex;
    flex-direction: column;
    gap: 6px;
    min-width: 0;
}

.line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 6px;
    min-height: 19px;
}

.loc,
.record,
.fix code {
    font-size: 12px;
    overflow-wrap: anywhere;
}

.record {
    line-height: 1.8;
}

.record-list {
    display: flex;
    flex-direction: column;
    gap: 2px;
    margin: 0;
    padding: 0;
    list-style: none;
}

.part {
    border-radius: 3px;

    &.problem {
        padding: 1px 3px;
        background: var(--danger-soft);
        text-decoration: underline solid var(--danger);
        text-decoration-thickness: 2px;
        text-underline-offset: 3px;
    }

    &.missing {
        padding: 0 3px;
        outline: 1px dashed var(--success);
        outline-offset: 1px;
        color: var(--success);

        &::before {
            content: '+';
            margin-right: 1px;
            font-weight: 600;
        }
    }
}

.fix {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: 6px 8px;
    font-size: 12px;
}

.fix-label {
    flex: none;
    min-width: 52px;
    color: var(--text-2);
    font-weight: 600;
    line-height: 26px;
}

.fix-warning {
    flex-basis: 100%;
    /* Under the record, past the Update: label. */
    padding-left: 60px;
    color: var(--warning);
    font-size: 11px;
    font-weight: 600;
}

.fix-text {
    color: var(--text-2);
}

.fix-value {
    display: flex;
    /* Sized to the record, so Copy sits right after it rather than at the card's edge. */
    flex: 0 1 auto;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
    padding-top: 4px;
}

.fix-name {
    color: var(--text-3);
    font-size: 11px;
    overflow-wrap: anywhere;
}

.fix code {
    color: var(--success);
}

.muted {
    color: var(--text-3);
    font-size: 12px;
}
</style>
