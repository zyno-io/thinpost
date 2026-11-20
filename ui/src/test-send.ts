import { showToast } from '@zyno-io/vue-foundation';
import { computed, onBeforeUnmount, ref, shallowRef, type Ref } from 'vue';
import { useRouter } from 'vue-router';

import { describeApiError, InvalidResponseError, type ApiErrorDescription } from './api-errors';
import { authState } from './auth';
import { DefaultApi, type RuntimeInfo, type TestMessageRequest } from './openapi-client-generated';
import { describeTestRefusal, isUnknownOutcome } from './test-message';
import { parseRuntimeInfo } from './tracking';

const MESSAGE_ID = /^[0-9a-f]{32}$/;

export interface TestSendOptions {
    appId: Readonly<Ref<string>>;
    /** Whether the app is active; a paused app refuses submissions. */
    isActive: Readonly<Ref<boolean>>;
    /** Called after an outcome that cannot be told; resolves true once the message list was reloaded. */
    onUnknownOutcome?: () => Promise<boolean>;
}

/**
 * State and actions for the "Send test message" dialog, shared by the screens that offer it.
 * The POST is never retried automatically, and after an unknown outcome a new send stays
 * blocked until the caller has reloaded the message list.
 */
export function useTestSend(options: TestSendOptions) {
    const router = useRouter();
    const isOpen = ref(false);
    const domains = shallowRef<string[] | null>(null);
    const domainsError = ref<ApiErrorDescription | null>(null);
    const preselect = ref<string | null>(null);
    const isSubmitting = ref(false);
    const error = ref<ApiErrorDescription | null>(null);
    /** Shown on the screen after the dialog closed on an unknown outcome. */
    const outcomeError = ref<ApiErrorDescription | null>(null);
    const blocked = ref(false);

    /** The runtime report, when it could be read; null means no early warning, not "unavailable". */
    const runtime = shallowRef<RuntimeInfo | null>(null);

    let seq = 0;
    let isUnmounted = false;
    onBeforeUnmount(() => {
        isUnmounted = true;
        seq++;
    });

    async function loadRuntime() {
        try {
            const { data } = await DefaultApi.getRuntime();
            if (!isUnmounted) runtime.value = parseRuntimeInfo(data);
        } catch {
            // The server re-checks everything on send; a missing report only means no early warning.
            if (!isUnmounted) runtime.value = null;
        }
    }
    void loadRuntime();

    /** The deployment's recipient policy when the server reported it; the allowlist bypass is offered only for 'allowlist'. */
    const recipientPolicy = computed(() => runtime.value?.features.recipient_policy ?? null);

    /** Why a test send is not offered, from what the console knows; the server re-checks everything. */
    const blocker = computed(() => {
        if (authState.status !== 'signed-in') return 'Sign in to send a test message.';
        if (!options.isActive.value) return 'This app is paused. Resume the app to send.';
        if (runtime.value?.features.submission === false) return 'Sending is not enabled on this Thinpost server.';
        if (blocked.value) {
            return options.onUnknownOutcome
                ? 'Wait for the message list to reload before sending another test message.'
                : 'Reload this page and check the messages before sending another test message.';
        }
        return null;
    });

    async function open(domain?: string) {
        if (blocked.value || isSubmitting.value) return;
        const mine = ++seq;
        const id = options.appId.value;
        isOpen.value = true;
        preselect.value = domain ?? null;
        domains.value = null;
        domainsError.value = null;
        error.value = null;
        outcomeError.value = null;
        try {
            const { data } = await DefaultApi.listAppDomains({ path: { appId: id } });
            if (isUnmounted || mine !== seq) return;
            const list = data?.domains;
            if (!Array.isArray(list)) throw new InvalidResponseError();
            domains.value = list.filter(d => d.active === true && typeof d.name === 'string').map(d => d.name);
        } catch (err) {
            if (isUnmounted || mine !== seq) return;
            domains.value = [];
            domainsError.value = describeApiError(err, { notFound: 'The app was not found.' });
        }
    }

    function close() {
        if (isSubmitting.value) return;
        seq++;
        isOpen.value = false;
    }

    async function submit(request: TestMessageRequest) {
        if (isSubmitting.value || blocked.value || authState.status !== 'signed-in') return;
        const id = options.appId.value;
        const mine = seq;
        isSubmitting.value = true;
        error.value = null;
        let unknown = false;
        try {
            const response = await DefaultApi.sendAppTestMessage({ path: { appId: id }, body: request });
            if (isUnmounted) return;
            const data = response?.data;
            if (response?.response?.status !== 201 || !data || typeof data.id !== 'string' || !MESSAGE_ID.test(data.id) || typeof data.accepted_at !== 'string') {
                throw new InvalidResponseError();
            }
            isOpen.value = false;
            showToast({ message: 'Test message queued', durationSecs: 3 });
            await router.push({ name: 'app-message', params: { appId: id, messageId: data.id } });
        } catch (err) {
            if (isUnmounted) return;
            if (isUnknownOutcome(err)) {
                // The message may have been queued. Never retry; make the operator look at the list first.
                unknown = true;
                blocked.value = true;
                isOpen.value = false;
                const description = describeApiError(err, { mutation: true });
                outcomeError.value = {
                    ...description,
                    message: `${description.message.replace(/ We can't tell whether the change went through\. Reload to check before trying again\.$/, '')} We can't tell whether the message was sent. Check the message list before sending another.`
                };
            } else if (mine === seq) {
                error.value = describeTestRefusal(err);
            }
        } finally {
            isSubmitting.value = false;
        }
        if (unknown && !isUnmounted && options.onUnknownOutcome && (await options.onUnknownOutcome()) && !isUnmounted) {
            blocked.value = false;
        }
    }

    return { isOpen, recipientPolicy, domains, domainsError, preselect, isSubmitting, error, outcomeError, blocker, open, close, submit };
}
