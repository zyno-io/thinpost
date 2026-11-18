import { showConfirm } from '@zyno-io/vue-foundation';
import { onBeforeUnmount, shallowRef } from 'vue';
import { onBeforeRouteLeave, onBeforeRouteUpdate } from 'vue-router';

import { registerLeaveGuard } from '@/auth';

export interface OneTimeSecretOptions {
    /** True while a request that will return a secret is in flight. */
    isPending: () => boolean;
    /** Leave prompt while a request is in flight. */
    pendingPrompt: { title: string; message: string };
    /** Leave prompt while the secret is displayed. */
    heldPrompt: { title: string; message: string };
}

/**
 * Holds a secret that the API returns exactly once. The secret lives only in
 * this component's memory (never in browser storage or the URL), is dropped on
 * dismissal and on unmount, and leaving the page (route change, sign-in or
 * sign-out, reload) while it is displayed or about to arrive asks first.
 * Call from `setup`.
 */
export function useOneTimeSecret<T extends object>(options: OneTimeSecretOptions) {
    const held = shallowRef<T | null>(null);
    let unmounted = false;

    function discard() {
        held.value = null;
    }

    async function confirmLeave() {
        if (options.isPending()) return showConfirm(options.pendingPrompt.title, options.pendingPrompt.message);
        if (!held.value) return true;
        const ok = await showConfirm(options.heldPrompt.title, options.heldPrompt.message);
        if (ok) discard();
        return ok;
    }

    onBeforeRouteLeave(confirmLeave);
    onBeforeRouteUpdate(confirmLeave);
    // Sign-in and sign-out leave the page without a route change.
    const unregisterLeaveGuard = registerLeaveGuard(confirmLeave);

    function onBeforeUnload(event: BeforeUnloadEvent) {
        if (held.value || options.isPending()) {
            event.preventDefault();
            event.returnValue = '';
        }
    }
    window.addEventListener('beforeunload', onBeforeUnload);

    onBeforeUnmount(() => {
        unmounted = true;
        unregisterLeaveGuard();
        window.removeEventListener('beforeunload', onBeforeUnload);
        discard();
    });

    return {
        /** The displayed secret, or null. */
        held,
        /** True once the component has unmounted; a late response must then drop its secret. */
        isUnmounted: () => unmounted,
        discard
    };
}
