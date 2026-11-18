import { defineConfig, devices } from '@playwright/test';
import process from 'node:process';

// Dev-only browser checks. Not part of `npm test`, `npm run build` or CI.
//
// Projects:
//  - dev: the Vite dev server (no CSP), for UI behaviour, layout and theme checks.
//  - csp: `vite preview` of the production build with the production console CSP injected by the
//    test fixture, for iframe-isolation and CSP-parity checks.
// Chromium always runs. Firefox and WebKit run the isolation specs when E2E_ALL_BROWSERS=1 and their
// browsers are installed (`npx playwright install firefox webkit`).

const DEV_PORT = 7935;
const CSP_PORT = 7936;
const allBrowsers = process.env.E2E_ALL_BROWSERS === '1';

const cspProject = (name: string, device: (typeof devices)[string]) => ({
    name,
    testMatch: /isolation\.spec\.ts/,
    use: { ...device, baseURL: `http://localhost:${CSP_PORT}`, extraHTTPHeaders: {} }
});

export default defineConfig({
    testDir: './tests/e2e',
    timeout: 30_000,
    expect: { timeout: 5000 },
    forbidOnly: !!process.env.CI,
    retries: 0,
    reporter: [['list'], ['html', { open: 'never' }]],
    use: { actionTimeout: 10_000, trace: 'retain-on-failure' },
    outputDir: 'test-results/',
    projects: [
        {
            name: 'dev',
            testIgnore: /isolation\.spec\.ts/,
            use: { ...devices['Desktop Chrome'], baseURL: `http://localhost:${DEV_PORT}` }
        },
        cspProject('csp-chromium', devices['Desktop Chrome']),
        ...(allBrowsers ? [cspProject('csp-firefox', devices['Desktop Firefox']), cspProject('csp-webkit', devices['Desktop Safari'])] : [])
    ],
    webServer: [
        { command: 'npm run dev', port: DEV_PORT, reuseExistingServer: true, timeout: 60_000 },
        {
            command: `npm run build && npx vite preview --port ${CSP_PORT} --strictPort`,
            port: CSP_PORT,
            reuseExistingServer: true,
            timeout: 180_000
        }
    ]
});
