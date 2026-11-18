import { afterEach, describe, expect, it, vi } from 'vitest';

vi.mock('@/openapi-client-generated', () => ({ DefaultApi: {} }));

const { default: router } = await import('../router');

const ID = '0123456789abcdef0123456789abcdef';

afterEach(() => {
    localStorage.clear();
});

describe('router', () => {
    it('drops OAuth callback parameters from the address bar', async () => {
        await router.push('/auth/callback?code=secret-code&state=s#x');
        expect(router.currentRoute.value.fullPath).toBe('/apps');
    });

    it('routes domains and messages to real screens', () => {
        expect(router.resolve('/domains').name).toBe('domains');
        expect(router.resolve('/messages').name).toBe('messages');
        const id = '0123456789abcdef0123456789abcdef';
        expect(router.resolve(`/apps/${id}/domains`).name).toBe('app-domains');
        expect(router.resolve(`/apps/${id}/messages`).name).toBe('app-messages');
        expect(router.resolve(`/apps/${id}/messages/${id}`).name).toBe('app-message');
    });

    it('routes webhooks to the per-app screen', () => {
        expect(router.resolve('/webhooks').name).toBe('webhooks');
        const id = '0123456789abcdef0123456789abcdef';
        expect(router.resolve(`/apps/${id}/webhooks`).name).toBe('app-webhooks');
    });

    it('routes tracking per app and operations globally', async () => {
        expect(router.resolve(`/apps/${ID}/tracking`).name).toBe('app-tracking');
        expect(router.resolve(`/apps/${ID}/tracking`).meta.section).toBe('tracking');
        expect(router.resolve('/operations').name).toBe('operations');
        expect(router.resolve('/operations').matched).toHaveLength(1);
        localStorage.setItem('thinpost:last-app', ID);
        await router.push('/tracking');
        expect(router.currentRoute.value.name).toBe('app-tracking');
        expect(router.currentRoute.value.params.appId).toBe(ID);
        localStorage.clear();
        await router.push('/apps');
        await router.push('/tracking');
        expect(router.currentRoute.value.name).toBe('tracking');
    });

    it('nests the app sections under the shell record', () => {
        for (const path of [`/apps/${ID}`, `/apps/${ID}/domains`, `/apps/${ID}/messages`, `/apps/${ID}/messages/${ID}`, `/apps/${ID}/webhooks`]) {
            const resolved = router.resolve(path);
            expect(resolved.matched).toHaveLength(2);
            expect(resolved.matched[0]!.path).toBe('/apps/:appId');
            expect(resolved.meta.section).toBeDefined();
        }
        expect(router.resolve('/apps').matched).toHaveLength(1);
    });

    it('sends global section links straight to the remembered app', async () => {
        localStorage.setItem('thinpost:last-app', ID);
        await router.push('/messages');
        expect(router.currentRoute.value.name).toBe('app-messages');
        expect(router.currentRoute.value.params.appId).toBe(ID);
        await router.push('/domains');
        expect(router.currentRoute.value.name).toBe('app-domains');
        await router.push('/webhooks');
        expect(router.currentRoute.value.name).toBe('app-webhooks');
    });

    it('keeps the picker when no app is remembered or the stored value is invalid', async () => {
        await router.push('/apps');
        await router.push('/messages');
        expect(router.currentRoute.value.name).toBe('messages');
        localStorage.setItem('thinpost:last-app', 'not-an-id');
        await router.push('/apps');
        await router.push('/domains');
        expect(router.currentRoute.value.name).toBe('domains');
    });
});
