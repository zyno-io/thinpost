import type { App } from 'vue';

import { configureVf, createFilters, installVf } from '@zyno-io/vue-foundation';

configureVf({
    unhandledErrorSupportText: 'contact your Thinpost administrator',
    // Request objects can carry bearer tokens; never log them.
    errorHandler: err => console.error('Unhandled error:', err.name, err.message)
});

export const filters = createFilters(() => ({}));

declare module 'vue' {
    export interface ComponentCustomProperties {
        $filters: typeof filters;
    }
}

export function setupVf(app: App) {
    installVf(app);
    app.config.globalProperties.$filters = filters;
}
