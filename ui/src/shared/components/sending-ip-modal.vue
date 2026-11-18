<template>
    <VfModal role="dialog" aria-modal="true" :aria-labelledby="`${id}-title`" class="tp-modal" @close="cancel" @form-submit="submit">
        <template #header>
            <span :id="`${id}-title`">{{ ip ? `Edit ${ip.name}` : 'Add a sending IP' }}</span>
        </template>

        <div class="modal-body">
            <ApiErrorNotice v-if="error" :error="error" />

            <label :for="`${id}-name`">Name</label>
            <input
                :id="`${id}-name`"
                v-model="name"
                v-autofocus
                class="input"
                type="text"
                autocomplete="off"
                spellcheck="false"
                maxlength="32"
                :aria-invalid="!!nameError && touched.name"
                :aria-describedby="`${id}-name-hint`"
                @blur="touched.name = true"
            />
            <p :id="`${id}-name-hint`" class="hint" :class="{ invalid: nameError && touched.name }">
                {{ nameError && touched.name ? nameError : 'A short name for this IP, such as app-mail or hosting. It appears in logs and in the console.' }}
            </p>

            <label :for="`${id}-address`">IP address</label>
            <p v-if="ip || lockedAddress" class="fixed" data-testid="fixed-address">
                <code>{{ ip?.address ?? lockedAddress }}</code>
            </p>
            <input
                v-else
                :id="`${id}-address`"
                v-model="address"
                class="input mono"
                type="text"
                inputmode="decimal"
                autocomplete="off"
                spellcheck="false"
                :aria-invalid="!!addressError && touched.address"
                :aria-describedby="`${id}-address-hint`"
                @blur="touched.address = true"
            />
            <p :id="`${id}-address-hint`" class="hint" :class="{ invalid: addressError && touched.address && !ip && !lockedAddress }">
                <template v-if="ip">An address can't be changed. To use a different address, add it as a new IP.</template>
                <template v-else-if="lockedAddress">This address was reported by the sending worker. Give it a name and its details.</template>
                <template v-else>{{ addressError && touched.address ? addressError : 'A public IPv4 address that the sending worker has been set up with. Adding it here doesn\'t give the worker the address.' }}</template>
            </p>

            <label :for="`${id}-helo`">HELO name</label>
            <input
                :id="`${id}-helo`"
                v-model="helo"
                class="input mono"
                type="text"
                autocomplete="off"
                spellcheck="false"
                :aria-invalid="!!heloError && touched.helo"
                :aria-describedby="`${id}-helo-hint`"
                @blur="touched.helo = true"
            />
            <p :id="`${id}-helo-hint`" class="hint" :class="{ invalid: heloError && touched.helo }">
                {{ heloError && touched.helo ? heloError : 'The name this address introduces itself with to receiving mail servers, such as smtp.example.com. Its reverse DNS should be the same name.' }}
            </p>

            <label :for="`${id}-bounce`">Bounce domain</label>
            <input
                :id="`${id}-bounce`"
                v-model="bounce"
                class="input mono"
                type="text"
                autocomplete="off"
                spellcheck="false"
                :aria-invalid="!!bounceError && touched.bounce"
                :aria-describedby="`${id}-bounce-hint`"
                @input="bounceChosen = true"
                @blur="touched.bounce = true"
            />
            <p :id="`${id}-bounce-hint`" class="hint" :class="{ invalid: bounceError && touched.bounce }">
                {{ bounceError && touched.bounce ? bounceError : 'Where bounce messages for mail sent from this address come back to. Use this IP\'s mail server name, or a domain it belongs to. Its SPF record must list this IP, and several IPs can share one.' }}
            </p>
        </div>

        <template #footer>
            <button type="button" class="btn" :disabled="isSubmitting" @click="cancel">Cancel</button>
            <button type="submit" class="btn primary" :disabled="isSubmitting || (!!ip && !changed)">{{ isSubmitting ? 'Saving…' : ip ? 'Save' : 'Add IP' }}</button>
        </template>
    </VfModal>
</template>

<script setup lang="ts">
import { VfModal } from '@zyno-io/vue-foundation';
import { computed, reactive, ref, useId, watch } from 'vue';

import type { ApiErrorDescription } from '@/api-errors';
import type { SendingIp } from '@/openapi-client-generated';
import { useModalPresence } from '@/shared/modal-state';
import { validateBounceForHelo, validateIpAddress, validateIpDomain, validateIpName } from '@/sending-ips';

import ApiErrorNotice from './api-error-notice.vue';

const props = defineProps<{
    /** The row being edited; absent when adding. */
    ip?: SendingIp;
    /** An address reported by the sending worker that has no name yet; it can't be changed. */
    lockedAddress?: string;
    isSubmitting: boolean;
    error: ApiErrorDescription | null;
}>();

const emit = defineEmits<{
    close: [];
    create: [input: { name: string; address: string; helo_name: string; bounce_domain: string }];
    update: [patch: { name?: string; helo_name?: string; bounce_domain?: string }];
}>();

useModalPresence();
const id = useId();
const name = ref(props.ip?.name ?? '');
const address = ref(props.lockedAddress ?? '');
const helo = ref(props.ip?.helo_name ?? '');
const bounce = ref(props.ip?.bounce_domain ?? '');
const touched = reactive({ name: false, address: false, helo: false, bounce: false });

const nameError = computed(() => validateIpName(name.value));
const addressError = computed(() => (props.ip || props.lockedAddress ? null : validateIpAddress(address.value)));
const heloError = computed(() => validateIpDomain(helo.value, 'HELO name', 253));
// Like the server, hold the pair to the rule only when adding, or when one of the two changed,
// so a row saved before the rule can still be renamed.
const pairChanged = computed(() => !props.ip || helo.value !== props.ip.helo_name || bounce.value !== props.ip.bounce_domain);
const bounceError = computed(() => validateIpDomain(bounce.value, 'bounce domain', 219) ?? (heloError.value || !pairChanged.value ? null : validateBounceForHelo(helo.value, bounce.value)));
// A new IP starts with its HELO name as the bounce domain, until someone chooses another.
const bounceChosen = ref(false);
watch(helo, value => {
    if (!props.ip && !bounceChosen.value) bounce.value = value;
});
const changed = computed(() => !props.ip || name.value !== props.ip.name || helo.value !== props.ip.helo_name || bounce.value !== props.ip.bounce_domain);

function cancel() {
    if (!props.isSubmitting) emit('close');
}

function submit() {
    touched.name = touched.address = touched.helo = touched.bounce = true;
    if (props.isSubmitting || nameError.value || addressError.value || heloError.value || bounceError.value) return;
    if (props.ip) {
        if (!changed.value) return;
        emit('update', {
            ...(name.value !== props.ip.name ? { name: name.value } : {}),
            ...(helo.value !== props.ip.helo_name ? { helo_name: helo.value } : {}),
            ...(bounce.value !== props.ip.bounce_domain ? { bounce_domain: bounce.value } : {})
        });
    } else {
        emit('create', { name: name.value, address: address.value, helo_name: helo.value, bounce_domain: bounce.value });
    }
}
</script>

<style lang="scss" scoped>
.modal-body {
    gap: 8px;
    width: min(520px, calc(100vw - 72px));

    label {
        margin-top: 4px;
        color: var(--text);
        font-weight: 600;
    }
}

.mono {
    font-family: var(--font-mono, monospace);
}

.fixed {
    margin: 0;
}

.hint {
    color: var(--text-3);
    font-size: 12px;

    &.invalid {
        color: var(--danger);
    }
}
</style>
