import { inject, provide, type ComputedRef, type InjectionKey, type Ref } from 'vue';

import type { App } from './openapi-client-generated';

export interface AppContext {
    /** ID of the ready app, never the raw route param. */
    appId: ComputedRef<string>;
    /** Non-null whenever section components render. */
    app: ComputedRef<App>;
    /** When the listApps result currently shown was received. */
    loadedAt: Readonly<Ref<Date>>;
    /** Refetches listApps; resolves to the app or null when missing; throws API errors; never flips the shell to not-found. */
    reload(): Promise<App | null>;
    /** Applies a status the API confirmed (for example pause succeeded but the reload failed). */
    update(app: App): void;
}

export const APP_CONTEXT: InjectionKey<AppContext> = Symbol('thinpost:app-context');

export function provideAppContext(context: AppContext): void {
    provide(APP_CONTEXT, context);
}

export function useAppContext(): AppContext {
    const context = inject(APP_CONTEXT, null);
    if (!context) throw new Error('useAppContext() must be used below the app shell.');
    return context;
}
