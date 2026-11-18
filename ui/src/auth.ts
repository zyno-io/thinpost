import { UserManager, WebStorageStateStore, type User } from 'oidc-client-ts';
import { reactive } from 'vue';

import { apiErrorStatus } from './api-errors';
import { DefaultApi } from './openapi-client-generated';

export const CALLBACK_PATH = '/auth/callback';

const OAUTH_RESPONSE_PARAMS = ['code', 'state', 'error', 'error_description', 'error_uri', 'iss', 'session_state'];

/**
 * - `initializing`: fetching the public OIDC configuration
 * - `unreachable`: the operator API did not provide its configuration
 * - `unconfigured`: the operator API reports that no valid identity provider is configured
 * - `signed-out`: an identity provider is configured; the operator has no session
 * - `redirecting`: handing off to the identity provider
 * - `signed-in`: a stored access token is held
 * - `ended`: a session existed but was rejected or expired; the shell stays mounted
 */
export type AuthStatus = 'initializing' | 'unreachable' | 'unconfigured' | 'signed-out' | 'redirecting' | 'signed-in' | 'ended';

export const authState = reactive({
    status: 'initializing' as AuthStatus,
    issuer: null as string | null,
    displayName: null as string | null,
    notice: null as string | null
});

// The access token lives in oidc-client-ts's localStorage user store (owner decision) and is kept out of reactive state.
const STORE_PREFIX = 'thinpost.oidc.';
const USER_KEY_PREFIX = STORE_PREFIX + 'user:';
let userKey = '';

/** Nothing uses a refresh token (silent renew is off), so it is never written to storage. */
export function stripRefreshToken(value: string): string {
    try {
        const record = JSON.parse(value) as unknown;
        if (record && typeof record === 'object' && 'refresh_token' in record) {
            delete (record as Record<string, unknown>).refresh_token;
            return JSON.stringify(record);
        }
    } catch {
        // Not a JSON record; store it unchanged.
    }
    return value;
}

class UserStore extends WebStorageStateStore {
    override set(key: string, value: string): Promise<void> {
        return super.set(key, stripRefreshToken(value));
    }
}

function readStored(raw: string | null): { access_token?: unknown; expires_at?: unknown } | null {
    if (!raw) return null;
    try {
        const record = JSON.parse(raw) as unknown;
        return record && typeof record === 'object' ? record : null;
    } catch {
        return null;
    }
}
let userManager: UserManager | null = null;
let currentUser: User | null = null;

export function getAccessToken(): string | null {
    if (!currentUser || currentUser.expired !== false) return null;
    return currentUser.access_token;
}

/**
 * Loads the public OIDC configuration and completes a pending sign-in
 * callback. Resolves to the in-app path to show after a completed callback.
 */
export async function initAuth(): Promise<string | null> {
    // Capture the callback URL before any await; the router may replace it.
    const callbackUrl = window.location.pathname === CALLBACK_PATH ? window.location.href : null;
    authState.status = 'initializing';
    authState.notice = null;
    userManager = null;
    currentUser = null;

    let config: unknown;
    try {
        const { data } = await DefaultApi.getPublicConfig();
        if (!data || typeof data !== 'object' || !('oidc' in data)) {
            authState.status = 'unreachable';
            authState.notice = 'Thinpost didn\'t return its sign-in settings. Check that this address points at Thinpost.';
            clearPendingCallback();
            return null;
        }
        config = data.oidc;
    } catch (err) {
        const status = apiErrorStatus(err);
        if (status === 503) {
            authState.status = 'unconfigured';
            authState.notice = 'Sign-in hasn\'t been set up for this Thinpost installation. Ask your Thinpost administrator to configure it.';
        } else {
            authState.status = 'unreachable';
            authState.notice =
                status === undefined
                    ? 'Couldn\'t reach Thinpost to load the sign-in settings.'
                    : 'Thinpost couldn\'t load the sign-in settings. Try again in a moment.';
        }
        clearPendingCallback();
        return null;
    }

    if (!isUsableConfig(config)) {
        authState.status = 'unconfigured';
        authState.notice = 'The sign-in settings for this Thinpost installation can\'t be used. Ask your Thinpost administrator to check them.';
        clearPendingCallback();
        return null;
    }

    authState.issuer = config.issuer;
    userManager = createUserManager(config);
    removeOrphanedUsers();

    if (!callbackUrl) {
        clearStaleState(userManager);
        // getUser() also schedules the access-token-expired timer for a restored user.
        const stored = await userManager.getUser().catch(() => null);
        if (stored && stored.expires_at !== undefined && !stored.expired) {
            setUser(stored);
            return null;
        }
        await discardSession();
        authState.status = 'signed-out';
        return null;
    }

    try {
        const user = await userManager.signinRedirectCallback(callbackUrl);
        if (user.expires_at === undefined) {
            // Without a token lifetime, expiry cannot be enforced; fail closed.
            await discardSession();
            authState.status = 'signed-out';
            authState.notice = 'Your sign-in provider didn\'t say how long your session lasts, so you couldn\'t be signed in. Ask your Thinpost administrator to check the provider.';
            return null;
        }
        setUser(user);
        return safeReturnPath((user.state as { returnTo?: unknown } | undefined)?.returnTo);
    } catch (err) {
        // A failed callback never stores a user, so nothing is removed here.
        authState.status = 'signed-out';
        const code = err && typeof err === 'object' && 'error' in err && typeof err.error === 'string' ? ` (${err.error})` : '';
        authState.notice = `Sign-in didn\'t complete${code}. Try again.`;
        clearPendingCallback();
        return null;
    } finally {
        clearStaleState(userManager);
    }
}

type LeaveGuard = () => Promise<boolean>;
const leaveGuards = new Set<LeaveGuard>();

/** Registers a check that must pass before sign-in or sign-out leaves the current page. */
export function registerLeaveGuard(guard: LeaveGuard): () => void {
    leaveGuards.add(guard);
    return () => leaveGuards.delete(guard);
}

async function confirmLeave(): Promise<boolean> {
    for (const guard of [...leaveGuards]) {
        const allowed = await guard();
        if (!allowed) return false;
    }
    return true;
}

export async function signIn(): Promise<void> {
    if (!userManager) return;
    const confirmed = await confirmLeave();
    if (!confirmed) return;
    const returnTo = safeReturnPath(window.location.pathname + window.location.search);
    // From an ended session, keep the shell mounted until the browser actually leaves.
    const fromEnded = authState.status === 'ended';
    if (!fromEnded) authState.status = 'redirecting';
    try {
        await userManager.signinRedirect({ state: { returnTo } });
    } catch {
        authState.status = fromEnded ? 'ended' : 'signed-out';
        authState.notice = 'Couldn\'t reach your sign-in provider. Try again in a moment.';
    }
}

export async function signOut(): Promise<void> {
    const confirmed = await confirmLeave();
    if (!confirmed) return;
    const manager = userManager;
    const idToken = currentUser?.id_token;
    await discardSession();
    authState.status = 'signed-out';
    authState.notice = 'You have signed out.';
    if (!manager) return;
    try {
        await manager.signoutRedirect(idToken ? { id_token_hint: idToken } : undefined);
    } catch {
        // The identity provider may not publish an end-session endpoint; the local session is already gone.
    }
}

/** Called when the operator API rejects the current token. */
export function handleUnauthorized(): void {
    if (authState.status !== 'signed-in') return;
    endSession('Your session is no longer valid. Sign in again to continue.');
}

export function safeReturnPath(value: unknown): string {
    if (typeof value !== 'string' || !value.startsWith('/') || value.startsWith('//') || value.startsWith('/\\')) return '/';
    try {
        const url = new URL(value, window.location.origin);
        if (url.origin !== window.location.origin || url.pathname === CALLBACK_PATH) return '/';
        for (const param of OAUTH_RESPONSE_PARAMS) url.searchParams.delete(param);
        return url.pathname + url.search;
    } catch {
        return '/';
    }
}

interface PublicOidcConfig {
    issuer: string;
    client_id: string;
    scopes: string[];
}

function isUsableConfig(config: unknown): config is PublicOidcConfig {
    if (!config || typeof config !== 'object') return false;
    const { issuer, client_id: clientId, scopes } = config as Record<string, unknown>;
    if (typeof issuer !== 'string' || typeof clientId !== 'string' || !clientId || !Array.isArray(scopes)) return false;
    if (!scopes.every(scope => typeof scope === 'string' && scope && !/\s/.test(scope)) || !scopes.includes('openid')) return false;
    try {
        return new URL(issuer).protocol === 'https:';
    } catch {
        return false;
    }
}

function createUserManager(config: PublicOidcConfig): UserManager {
    const origin = window.location.origin;
    userKey = `${USER_KEY_PREFIX}${config.issuer}:${config.client_id}`;
    const manager = new UserManager({
        authority: config.issuer,
        client_id: config.client_id,
        redirect_uri: origin + CALLBACK_PATH,
        post_logout_redirect_uri: origin + '/',
        response_type: 'code',
        scope: config.scopes.join(' '),
        // Authorization code with PKCE. The PKCE verifier and state live in
        // sessionStorage only for the redirect round trip; the signed-in user is kept in localStorage (see userStore).
        stateStore: new WebStorageStateStore({ store: window.sessionStorage, prefix: STORE_PREFIX }),
        userStore: new UserStore({ store: window.localStorage, prefix: STORE_PREFIX }),
        automaticSilentRenew: false,
        monitorSession: false,
        loadUserInfo: false
    });
    manager.events.addAccessTokenExpired(() => {
        if (authState.status === 'signed-in') endSession('Your session expired. Sign in again to continue.');
    });
    return manager;
}

// Tabs converge on the single stored user. Another tab removing it ends this session (memory only; the
// entry is already gone); another tab storing a newer valid user is adopted through the manager so the
// expiry timer re-arms.
window.addEventListener('storage', event => {
    if (event.storageArea !== window.localStorage || (event.key !== null && event.key !== userKey) || !userKey) return;
    if (event.key === null || event.newValue === null) {
        if (authState.status !== 'signed-in') return;
        currentUser = null;
        authState.displayName = null;
        authState.status = 'ended';
        authState.notice = 'You were signed out in another tab. Sign in again to continue.';
        return;
    }
    const record = readStored(event.newValue);
    const manager = userManager;
    if (!manager || !record || typeof record.expires_at !== 'number' || record.expires_at * 1000 <= Date.now()) return;
    if (record.access_token === currentUser?.access_token) return;
    if (!['signed-in', 'ended', 'signed-out'].includes(authState.status)) return;
    void manager
        .getUser()
        .then(user => {
            if (user && user.expires_at !== undefined && !user.expired) setUser(user);
        })
        .catch(() => undefined);
});

/** Removes stored users left behind by a previous issuer or client ID. */
function removeOrphanedUsers() {
    for (const key of Object.keys(window.localStorage)) {
        if (key.startsWith(USER_KEY_PREFIX) && key !== userKey) window.localStorage.removeItem(key);
    }
}

/** Removes abandoned sign-in state; run only after any pending callback has consumed its own state. */
function clearStaleState(manager: UserManager) {
    void manager.clearStaleState().catch(() => undefined);
}

function setUser(user: User) {
    currentUser = user;
    const profile = user.profile;
    authState.displayName = [profile.name, profile.preferred_username, profile.email].find(v => typeof v === 'string' && v) ?? null;
    authState.status = 'signed-in';
    authState.notice = null;
}

/** Ends this tab's session without disturbing a different user another tab may have stored since. */
function endSession(notice: string) {
    if (currentUser && readStored(window.localStorage.getItem(userKey))?.access_token === currentUser.access_token) {
        window.localStorage.removeItem(userKey);
    }
    currentUser = null;
    authState.displayName = null;
    authState.status = 'ended';
    authState.notice = notice;
}

async function discardSession(): Promise<void> {
    currentUser = null;
    authState.displayName = null;
    await userManager?.removeUser().catch(() => undefined);
}

function clearPendingCallback() {
    if (window.location.pathname === CALLBACK_PATH) window.history.replaceState(null, '', '/');
}
