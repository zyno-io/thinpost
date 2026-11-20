// Dev-only light/dark screenshot smoke. Screenshots go to test-results/screenshots and are for human review;
// the assertions only prove each screen rendered visible, non-empty content in both schemes and widths.
import { expect, test } from '@playwright/test';

import { APP_ID, installMocks, MSG_BARE, MSG_EP, MSG_OLD, MSG_PLAIN, openSignedIn } from './fixtures/console';

const SCREENS: Array<{ name: string; path: string }> = [
    { name: 'apps', path: '/apps' },
    { name: 'keys', path: `/apps/${APP_ID}` },
    { name: 'domains', path: `/apps/${APP_ID}/domains` },
    { name: 'messages', path: `/apps/${APP_ID}/messages` },
    { name: 'message-detail', path: `/apps/${APP_ID}/messages/${MSG_PLAIN}` },
    { name: 'message-detail-ep', path: `/apps/${APP_ID}/messages/${MSG_EP}` },
    { name: 'message-detail-retrying', path: `/apps/${APP_ID}/messages/${MSG_OLD}` },
    { name: 'message-detail-unrecorded', path: `/apps/${APP_ID}/messages/${MSG_BARE}` },
    { name: 'webhooks', path: `/apps/${APP_ID}/webhooks` },
    { name: 'suppressions', path: `/apps/${APP_ID}/suppressions` },
    { name: 'tracking', path: `/apps/${APP_ID}/tracking` },
    { name: 'operations', path: '/operations' },
    { name: 'not-found', path: '/missing/page' }
];

for (const colorScheme of ['light', 'dark'] as const) {
    for (const width of [1280, 360]) {
        test(`screenshot smoke: ${colorScheme} ${width}px`, async ({ page }, testInfo) => {
            await installMocks(page);
            await page.emulateMedia({ colorScheme });
            await page.setViewportSize({ width, height: 900 });

            // Sign-in screen first, before a session exists.
            await page.goto('/');
            await expect(page.getByRole('button', { name: 'Sign in' })).toBeVisible();
            await page.screenshot({ path: testInfo.outputPath(`signin-${colorScheme}-${width}.png`) });

            for (const screen of SCREENS) {
                await openSignedIn(page, screen.path);
                await expect(page.locator('main#main-content')).toContainText(/\S/);
                // Let content and the sandboxed preview settle.
                await page.waitForLoadState('networkidle');
                const shot = await page.screenshot({ path: testInfo.outputPath(`${screen.name}-${colorScheme}-${width}.png`), fullPage: true });
                expect(shot.length).toBeGreaterThan(5_000);
            }

            // Modal and tab states.
            await openSignedIn(page, `/apps/${APP_ID}`);
            await page.getByRole('button', { name: 'Create key' }).click();
            await expect(page.locator('.vf-modal')).toBeVisible();
            await page.screenshot({ path: testInfo.outputPath(`keys-modal-${colorScheme}-${width}.png`) });
            await page.getByRole('button', { name: 'Cancel' }).click();

            await openSignedIn(page, `/apps/${APP_ID}/webhooks`);
            await page.getByRole('button', { name: 'Add endpoint' }).click();
            await page.locator('.vf-modal select').selectOption('postmark');
            await page.getByRole('button', { name: /^HTTP authentication/ }).click();
            await page.getByLabel('User name').fill('thinpost');
            await page.getByLabel('Password').fill('fabricated-e2e-password');
            await page.getByRole('button', { name: 'Add header' }).click();
            await page.getByLabel('Header 1 name').fill('X-Api-Key');
            await page.getByLabel('Value').fill('fabricated-e2e-value');
            await page.getByLabel('Endpoint URL').fill('https://hooks.example.com/postmark');
            await expect(page.locator('.vf-modal')).toBeVisible();
            await page.screenshot({ path: testInfo.outputPath(`webhook-modal-${colorScheme}-${width}.png`) });
            await page.getByRole('button', { name: 'Cancel' }).click();

            await openSignedIn(page, `/apps/${APP_ID}/suppressions`);
            await page.getByRole('button', { name: 'Add', exact: true }).click();
            await page.locator('.vf-modal textarea').fill('one@example.com\ntwo@example.com\nnot valid');
            await expect(page.locator('.vf-modal')).toBeVisible();
            await page.screenshot({ path: testInfo.outputPath(`suppressions-modal-${colorScheme}-${width}.png`) });
            await page.getByRole('button', { name: 'Cancel' }).click();

            await openSignedIn(page, `/apps/${APP_ID}/tracking`);
            await page.getByRole('button', { name: 'Edit' }).click();
            await expect(page.locator('.vf-modal')).toBeVisible();
            await page.screenshot({ path: testInfo.outputPath(`tracking-modal-${colorScheme}-${width}.png`) });
            await page.getByRole('button', { name: 'Cancel' }).click();

            await openSignedIn(page, '/operations');
            await page.getByRole('button', { name: 'Edit' }).click();
            await expect(page.locator('.vf-modal')).toBeVisible();
            await page.screenshot({ path: testInfo.outputPath(`retry-policy-modal-${colorScheme}-${width}.png`) });
            await page.getByRole('button', { name: 'Cancel' }).click();

            await openSignedIn(page, `/apps/${APP_ID}/messages/${MSG_EP}`);
            await page.getByRole('button', { name: 'About Enhanced Protection' }).click();
            await expect(page.getByRole('dialog')).toBeVisible();
            await page.screenshot({ path: testInfo.outputPath(`help-modal-${colorScheme}-${width}.png`) });
            await page.keyboard.press('Escape');
            await expect(page.getByRole('dialog')).toHaveCount(0);

            await openSignedIn(page, `/apps/${APP_ID}/messages/${MSG_PLAIN}`);
            for (const tab of ['Text', 'HTML source']) {
                await page.getByRole('tab', { name: tab }).click();
                await page.screenshot({ path: testInfo.outputPath(`message-${tab.replace(' ', '-')}-${colorScheme}-${width}.png`) });
            }
            await page.getByRole('button', { name: /^Headers/ }).first().click();
            await expect(page.getByRole('dialog')).toBeVisible();
            await page.screenshot({ path: testInfo.outputPath(`headers-modal-${colorScheme}-${width}.png`) });
        });
    }
}
