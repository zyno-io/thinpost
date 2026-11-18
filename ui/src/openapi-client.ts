import { OpenApiError } from '@zyno-io/openapi-client-codegen';
import { configureVfOpenApiClient } from '@zyno-io/vue-foundation';

import { getAccessToken, handleUnauthorized } from './auth';
import { client } from './openapi-client-generated/client.gen';

// The operator API is served by the control plane on the same origin.
client.setConfig({
    baseUrl: '/api/v1',
    cache: 'no-store',
    credentials: 'omit'
});

configureVfOpenApiClient(client, {
    headers() {
        const token = getAccessToken();
        return token ? { Authorization: `Bearer ${token}` } : {};
    },

    onError(err) {
        if (err instanceof OpenApiError && err.response?.status === 401) {
            handleUnauthorized();
        }
        return err;
    }
});
