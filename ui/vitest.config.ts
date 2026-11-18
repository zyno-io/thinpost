import { fileURLToPath } from 'node:url';
import { mergeConfig } from 'vite';
import { configDefaults, defineConfig } from 'vitest/config';

import viteConfig from './vite.config.js';

export default mergeConfig(
    viteConfig,
    defineConfig({
        test: {
            environment: 'happy-dom',
            exclude: [...configDefaults.exclude],
            include: ['src/**/__tests__/*.spec.ts'],
            root: fileURLToPath(new URL('./', import.meta.url)),
            server: {
                deps: {
                    inline: ['@zyno-io/vue-foundation']
                }
            }
        }
    })
);
