import vue from '@vitejs/plugin-vue';
import { openapiClientGeneratorPlugin } from '@zyno-io/vue-foundation/vite-plugins';
import { fileURLToPath, URL } from 'node:url';
import { defineConfig } from 'vite';

export default defineConfig({
    css: {
        preprocessorOptions: {
            scss: {
                api: 'modern-compiler'
            }
        }
    },
    server: {
        port: 7935,
        // Optional: forward /api to a locally running control plane, e.g. THINPOST_API_URL=http://localhost:8080
        proxy: process.env.THINPOST_API_URL ? { '/api': { target: process.env.THINPOST_API_URL } } : undefined
    },
    build: {
        sourcemap: true,
        target: 'esnext'
    },
    plugins: [vue(), openapiClientGeneratorPlugin()],
    resolve: {
        alias: {
            '@': fileURLToPath(new URL('./src', import.meta.url))
        }
    }
});
