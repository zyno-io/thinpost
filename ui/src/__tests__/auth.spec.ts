import { beforeEach, describe, expect, it, vi } from 'vitest';

import { httpError, networkError } from './helpers';

const getPublicConfig = vi.fn();
vi.mock('../openapi-client-generated', () => ({ DefaultApi: { getPublicConfig } }));

let callbackResult: () => Promise<unknown> = async () => {
    throw new Error('unexpected callback');
};
let storedUser: Record<string, unknown> | null = null;
const USER_KEY = 'thinpost.oidc.user:https://idp.example/tenant:thinpost-operator';
const managers: FakeUserManager[] = [];
class FakeUserManager {
    settings: Record<string, unknown>;
    signinRedirect = vi.fn(async (_args?: unknown) => undefined);
    signinRedirectCallback = vi.fn((_url?: string) => callbackResult());
    getUser = vi.fn(async () => storedUser);
    removeUser = vi.fn(async () => {
        storedUser = null;
        localStorage.removeItem(USER_KEY);
    });
    clearStaleState = vi.fn(async () => undefined);
    events = { addAccessTokenExpired: vi.fn() };
    constructor(settings: Record<string, unknown>) {
        this.settings = settings;
        managers.push(this);
    }
}
vi.mock('oidc-client-ts', () => ({
    UserManager: FakeUserManager,
    WebStorageStateStore: class {
        constructor(public options: { store: Storage; prefix: string }) {}
        async set(key: string, value: string) {
            this.options.store.setItem(this.options.prefix + key, value);
        }
    }
}));

const { stripRefreshToken, authState, getAccessToken, handleUnauthorized, initAuth, registerLeaveGuard, safeReturnPath, signIn, signOut } = await import('../auth');

const validConfig = { oidc: { issuer: 'https://idp.example/tenant', client_id: 'thinpost-operator', scopes: ['openid', 'api://thinpost/admin'] } };

beforeEach(() => {
    managers.length = 0;
    storedUser = null;
    getPublicConfig.mockReset();
    localStorage.clear();
    sessionStorage.clear();
    window.history.replaceState(null, '', '/');
});

describe('initAuth', () => {
    it('reports an unreachable API when the config request gets no response', async () => {
        getPublicConfig.mockRejectedValue(networkError());
        await initAuth();
        expect(authState.status).toBe('unreachable');
        expect(managers).toHaveLength(0);
    });

    it('treats a non-API response (such as an HTML fallback) as unreachable', async () => {
        getPublicConfig.mockResolvedValue({ data: '<!doctype html>' });
        await initAuth();
        expect(authState.status).toBe('unreachable');
    });

    it('reports missing identity provider configuration on 503', async () => {
        getPublicConfig.mockRejectedValue(httpError(503));
        await initAuth();
        expect(authState.status).toBe('unconfigured');
    });

    it('refuses a configuration with a non-HTTPS issuer or without openid', async () => {
        getPublicConfig.mockResolvedValue({ data: { oidc: { ...validConfig.oidc, issuer: 'http://idp.example' } } });
        await initAuth();
        expect(authState.status).toBe('unconfigured');

        getPublicConfig.mockResolvedValue({ data: { oidc: { ...validConfig.oidc, scopes: ['profile'] } } });
        await initAuth();
        expect(authState.status).toBe('unconfigured');
        expect(managers).toHaveLength(0);
    });

    it('configures authorization code flow with the user in localStorage', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        await initAuth();
        expect(authState.status).toBe('signed-out');
        const settings = managers[0]!.settings;
        expect(settings).toMatchObject({
            authority: 'https://idp.example/tenant',
            client_id: 'thinpost-operator',
            response_type: 'code',
            scope: 'openid api://thinpost/admin',
            redirect_uri: `${window.location.origin}/auth/callback`,
            automaticSilentRenew: false
        });
        const userStore = settings.userStore as { options: { store: unknown } };
        const stateStore = settings.stateStore as { options: { store: unknown } };
        expect(userStore.options.store).toBe(localStorage);
        expect(stateStore.options.store).toBe(sessionStorage);
    });

    it('completes a callback and returns a safe path', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        callbackResult = async () => ({
            access_token: 'access-token-value',
            expires_at: Math.floor(Date.now() / 1000) + 3600,
            expired: false,
            profile: { name: 'Operator' },
            state: { returnTo: '/apps/abc' }
        });
        window.history.replaceState(null, '', '/auth/callback?code=c&state=s');
        const pending = initAuth();
        // The router may replace the URL while the config request is pending.
        window.history.replaceState(null, '', '/apps');

        await expect(pending).resolves.toBe('/apps/abc');
        expect(managers[0]!.signinRedirectCallback).toHaveBeenCalledWith(expect.stringContaining('/auth/callback?code=c&state=s'));
        expect(authState.status).toBe('signed-in');
        expect(authState.displayName).toBe('Operator');
        expect(getAccessToken()).toBe('access-token-value');
        expect(JSON.stringify({ ...sessionStorage })).not.toContain('access-token-value');

        // The real store writes the user to localStorage; simulate it, then confirm a 401 removes it.
        localStorage.setItem(USER_KEY, JSON.stringify({ access_token: 'access-token-value' }));
        handleUnauthorized();
        expect(authState.status).toBe('ended');
        expect(getAccessToken()).toBeNull();
        expect(localStorage.getItem(USER_KEY)).toBeNull();
    });

    const validUser = (over: Record<string, unknown> = {}) => ({
        access_token: 'restored-token',
        expires_at: Math.floor(Date.now() / 1000) + 3600,
        expired: false,
        profile: { name: 'Operator' },
        ...over
    });

    it('restores a valid stored user without a callback', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        storedUser = validUser();
        window.history.replaceState(null, '', '/apps/abc');
        await expect(initAuth()).resolves.toBeNull();
        expect(authState.status).toBe('signed-in');
        expect(authState.displayName).toBe('Operator');
        expect(getAccessToken()).toBe('restored-token');
        expect(window.location.pathname).toBe('/apps/abc');
    });

    it('discards an expired stored user and signs out', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        storedUser = validUser({ expires_at: 1, expired: true });
        localStorage.setItem(USER_KEY, 'x');
        await initAuth();
        expect(authState.status).toBe('signed-out');
        expect(getAccessToken()).toBeNull();
        expect(managers[0]!.removeUser).toHaveBeenCalled();
        expect(localStorage.getItem(USER_KEY)).toBeNull();
    });

    it('discards a stored user without a token lifetime', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        storedUser = validUser({ expires_at: undefined, expired: undefined });
        await initAuth();
        expect(authState.status).toBe('signed-out');
        expect(managers[0]!.removeUser).toHaveBeenCalled();
    });

    it('ends the session when the access token expires', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        storedUser = validUser();
        await initAuth();
        const onExpired = managers[0]!.events.addAccessTokenExpired.mock.calls[0]![0] as () => void;
        onExpired();
        expect(authState.status).toBe('ended');
        expect(authState.notice).toContain('expired');
        expect(getAccessToken()).toBeNull();
    });

    it('expiry or a 401 removes this tab\'s own stored user but keeps a newer one from another tab', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        storedUser = validUser();
        await initAuth();
        localStorage.setItem(USER_KEY, JSON.stringify({ access_token: 'restored-token' }));
        handleUnauthorized();
        expect(localStorage.getItem(USER_KEY)).toBeNull();

        storedUser = validUser();
        await initAuth();
        const newer = JSON.stringify({ access_token: 'other-tab-token', expires_at: Math.floor(Date.now() / 1000) + 3600 });
        localStorage.setItem(USER_KEY, newer);
        const onExpired = managers[1]!.events.addAccessTokenExpired.mock.calls[0]![0] as () => void;
        onExpired();
        expect(authState.status).toBe('ended');
        expect(localStorage.getItem(USER_KEY)).toBe(newer);
        expect(managers[1]!.removeUser).not.toHaveBeenCalled();
    });

    it('does not remove a stored user when the callback fails', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        callbackResult = async () => {
            throw new Error('bad');
        };
        localStorage.setItem(USER_KEY, 'other');
        window.history.replaceState(null, '', '/auth/callback?code=c&state=s');
        await initAuth();
        expect(managers[0]!.removeUser).not.toHaveBeenCalled();
        expect(localStorage.getItem(USER_KEY)).toBe('other');
    });

    it('removes stored users left by another issuer or client on init', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        localStorage.setItem('thinpost.oidc.user:https://old.example:old', '{}');
        localStorage.setItem('thinpost:theme', 'dark');
        await initAuth();
        expect(localStorage.getItem('thinpost.oidc.user:https://old.example:old')).toBeNull();
        expect(localStorage.getItem('thinpost:theme')).toBe('dark');
    });

    it('adopts a newer valid user stored by another tab', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        await initAuth();
        expect(authState.status).toBe('signed-out');
        storedUser = validUser({ access_token: 'other-tab-token' });
        window.dispatchEvent(
            new StorageEvent('storage', {
                key: USER_KEY,
                newValue: JSON.stringify({ access_token: 'other-tab-token', expires_at: Math.floor(Date.now() / 1000) + 3600 }),
                storageArea: localStorage
            })
        );
        await vi.waitFor(() => expect(authState.status).toBe('signed-in'));
        expect(getAccessToken()).toBe('other-tab-token');
        expect(managers[0]!.getUser).toHaveBeenCalled();
    });

    it('ignores another tab storing an expired user', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        await initAuth();
        window.dispatchEvent(new StorageEvent('storage', { key: USER_KEY, newValue: JSON.stringify({ access_token: 'old', expires_at: 1 }), storageArea: localStorage }));
        expect(authState.status).toBe('signed-out');
    });

    it('strips refresh tokens before the user is written', async () => {
        expect(JSON.parse(stripRefreshToken(JSON.stringify({ access_token: 'a', refresh_token: 'r', expires_at: 5 })))).toEqual({ access_token: 'a', expires_at: 5 });
        expect(stripRefreshToken('not json')).toBe('not json');
        getPublicConfig.mockResolvedValue({ data: validConfig });
        await initAuth();
        const store = managers[0]!.settings.userStore as { set(key: string, value: string): Promise<void> };
        await store.set('user:k', JSON.stringify({ access_token: 'a', refresh_token: 'secret' }));
        expect(localStorage.getItem('thinpost.oidc.user:k')).not.toContain('secret');
        expect(localStorage.getItem('thinpost.oidc.user:k')).toContain('access_token');
    });

    it('ends the session when another tab removes the stored user', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        storedUser = validUser();
        await initAuth();
        window.dispatchEvent(new StorageEvent('storage', { key: 'unrelated', newValue: null, storageArea: localStorage }));
        expect(authState.status).toBe('signed-in');
        window.dispatchEvent(new StorageEvent('storage', { key: USER_KEY, newValue: null, storageArea: localStorage }));
        expect(authState.status).toBe('ended');
        expect(getAccessToken()).toBeNull();
        expect(managers[0]!.removeUser).not.toHaveBeenCalled();
    });

    it('removes the stored user on sign-out', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        storedUser = validUser();
        await initAuth();
        localStorage.setItem(USER_KEY, 'x');
        await signOut();
        expect(authState.status).toBe('signed-out');
        expect(localStorage.getItem(USER_KEY)).toBeNull();
    });

    it('does not use an expired token', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        callbackResult = async () => ({ access_token: 'old', expires_at: 1, expired: true, profile: {} });
        window.history.replaceState(null, '', '/auth/callback?code=c&state=s');
        await initAuth();
        expect(getAccessToken()).toBeNull();
    });

    it('fails closed when the identity provider reports no token lifetime', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        callbackResult = async () => ({ access_token: 'no-expiry', expired: undefined, profile: {} });
        window.history.replaceState(null, '', '/auth/callback?code=c&state=s');
        await expect(initAuth()).resolves.toBeNull();
        expect(authState.status).toBe('signed-out');
        expect(managers[0]!.removeUser).toHaveBeenCalled();
        expect(authState.notice).toContain("didn't say how long your session lasts");
        expect(getAccessToken()).toBeNull();
    });

    it('stays signed out when the callback fails', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        callbackResult = async () => {
            throw Object.assign(new Error('denied'), { error: 'access_denied' });
        };
        window.history.replaceState(null, '', '/auth/callback?error=access_denied&state=s');
        await expect(initAuth()).resolves.toBeNull();
        expect(authState.status).toBe('signed-out');
        expect(authState.notice).toContain('access_denied');
        expect(getAccessToken()).toBeNull();
        expect(window.location.pathname).toBe('/');
    });
});

describe('leave guards', () => {
    it('block sign-in and sign-out until confirmed', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        await initAuth();
        const guard = vi.fn(async () => false);
        const unregister = registerLeaveGuard(guard);

        await signIn();
        expect(managers[0]!.signinRedirect).not.toHaveBeenCalled();
        expect(authState.status).toBe('signed-out');

        authState.status = 'ended';
        await signOut();
        expect(authState.status).toBe('ended');
        expect(guard).toHaveBeenCalledTimes(2);

        unregister();
        await signIn();
        expect(managers[0]!.signinRedirect).toHaveBeenCalled();
        // From an ended session the shell stays mounted while redirecting.
        expect(authState.status).toBe('ended');
    });
});

describe('signIn', () => {
    it('starts a redirect carrying the current in-app path', async () => {
        getPublicConfig.mockResolvedValue({ data: validConfig });
        window.history.replaceState(null, '', '/apps/0123?x=1');
        await initAuth();
        await signIn();
        expect(managers[0]!.signinRedirect).toHaveBeenCalledWith({ state: { returnTo: '/apps/0123?x=1' } });
    });
});

describe('safeReturnPath', () => {
    it.each(['//evil.example/', '/\\evil.example', 'https://evil.example/', 'apps', '/auth/callback?code=x', 42, undefined])('rejects %s', value => {
        expect(safeReturnPath(value)).toBe('/');
    });

    it('drops OAuth response parameters', () => {
        expect(safeReturnPath('/apps?code=c&state=s&error=e&iss=i&tab=keys')).toBe('/apps?tab=keys');
    });

    it('keeps same-origin paths', () => {
        expect(safeReturnPath('/apps/abc?tab=keys')).toBe('/apps/abc?tab=keys');
    });
});
