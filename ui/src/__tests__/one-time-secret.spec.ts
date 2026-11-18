import { flushPromises, mount, type VueWrapper } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { defineComponent, h } from 'vue';
import { createMemoryHistory, createRouter } from 'vue-router';

const vf = vi.hoisted(() => ({ showConfirm: vi.fn() }));
vi.mock('@zyno-io/vue-foundation', async importOriginal => ({ ...(await importOriginal<object>()), ...vf }));

const { authState, signOut } = await import('@/auth');
const { useOneTimeSecret } = await import('@/shared/one-time-secret');

let pending = false;
let api: ReturnType<typeof useOneTimeSecret<{ secret: string }>>;
let wrapper: VueWrapper | undefined;

const Holder = defineComponent({
    setup() {
        api = useOneTimeSecret<{ secret: string }>({
            isPending: () => pending,
            pendingPrompt: { title: 'pending', message: 'pending message' },
            heldPrompt: { title: 'held', message: 'held message' }
        });
        return () => h('div');
    }
});

function beforeUnload() {
    const event = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
}

beforeEach(async () => {
    vf.showConfirm.mockReset();
    pending = false;
    authState.status = 'signed-in';
    const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/', component: Holder }] });
    await router.push('/');
    wrapper = mount({ template: '<RouterView />' }, { global: { plugins: [router] } });
    await flushPromises();
});

afterEach(() => {
    wrapper?.unmount();
    wrapper = undefined;
});

describe('useOneTimeSecret leave guards', () => {
    it('does not interfere when nothing is pending or held', async () => {
        expect(beforeUnload()).toBe(false);
        await signOut();
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(authState.status).toBe('signed-out');
    });

    it('prompts on sign-out and prevents unload while a request is pending', async () => {
        pending = true;
        expect(beforeUnload()).toBe(true);
        vf.showConfirm.mockResolvedValueOnce(false);
        await signOut();
        expect(vf.showConfirm).toHaveBeenCalledWith('pending', 'pending message');
        expect(authState.status).toBe('signed-in');
    });

    it('keeps a held secret when sign-out is declined and drops it when confirmed', async () => {
        api.held.value = { secret: 'whsec_x' };
        expect(beforeUnload()).toBe(true);
        vf.showConfirm.mockResolvedValueOnce(false);
        await signOut();
        expect(vf.showConfirm).toHaveBeenCalledWith('held', 'held message');
        expect(authState.status).toBe('signed-in');
        expect(api.held.value).toEqual({ secret: 'whsec_x' });

        vf.showConfirm.mockResolvedValueOnce(true);
        await signOut();
        expect(authState.status).toBe('signed-out');
        expect(api.held.value).toBeNull();
    });

    it('removes every guard and drops the secret on unmount', async () => {
        api.held.value = { secret: 'whsec_x' };
        pending = true;
        wrapper!.unmount();
        wrapper = undefined;
        expect(api.held.value).toBeNull();
        expect(api.isUnmounted()).toBe(true);
        expect(beforeUnload()).toBe(false);
        await signOut();
        expect(vf.showConfirm).not.toHaveBeenCalled();
        expect(authState.status).toBe('signed-out');
    });
});
