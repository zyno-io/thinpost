// Dev-only checks against the Vite dev server with fabricated API responses (see fixtures/console.ts).
import { expect, test } from '@playwright/test';

import { APP_ID, installMocks, MSG_OLD, openSignedIn, recordRequests, SMTP_PASSWORD } from './fixtures/console';

test.beforeEach(async ({ page }) => {
    await installMocks(page);
});

test('the SMTP tab connects, creates a credential and saves a From policy', async ({ page }) => {
    const requests = recordRequests(page);
    await openSignedIn(page, `/apps/${APP_ID}`);
    await page.getByRole('navigation', { name: 'App sections' }).getByRole('link', { name: 'SMTP' }).click();
    await expect(page).toHaveURL(new RegExp(`/apps/${APP_ID}/smtp$`));

    const connect = page.getByTestId('smtp-connect');
    await expect(connect.getByLabel('Server')).toHaveValue('smtp.example.test');
    await expect(connect.getByLabel('Port 465')).toHaveValue('465');
    await expect(connect.getByLabel('Port 587')).toHaveValue('587');
    await expect(page.getByTestId('smtp-port-465-status')).toHaveText('Available');
    await expect(page.getByTestId('smtp-recommendation')).toContainText('port 465');

    await page.getByRole('button', { name: 'Create credential' }).click();
    const dialog = page.getByRole('dialog');
    await dialog.getByLabel('Credential name').fill('Warehouse scanner');
    await dialog.getByRole('button', { name: 'Create credential' }).click();
    const secret = page.getByRole('alertdialog');
    await expect(secret.getByLabel('Password')).toHaveValue(SMTP_PASSWORD);
    await expect(secret.getByLabel('Username')).toHaveValue('b8'.repeat(16));
    // The password is held in memory only.
    expect(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))).not.toContain(SMTP_PASSWORD);
    await secret.getByRole('button', { name: 'Done' }).click();
    // Closing without copying asks first; confirm it so the password is dropped.
    const confirm = page.locator('.vf-modal-wrap.vf-alert');
    await confirm.waitFor({ state: 'visible' });
    await confirm.getByRole('button', { name: 'Confirm' }).click();
    await expect(page.getByRole('alertdialog')).toHaveCount(0);
    await expect(page.locator('body')).not.toContainText(SMTP_PASSWORD);

    await page.getByTestId('smtp-mode-domain').check();
    await page.locator('#smtp-from-domain').selectOption('billing.example.com');
    await expect(page.getByTestId('smtp-examples')).toContainText('printer@billing.example.com');
    await page.getByTestId('smtp-save').click();
    await expect(page.getByTestId('smtp-signing-warning')).toContainText('billing.example.com');
    const put = requests.find(r => r.method === 'PUT' && r.url.endsWith('/smtp/settings'));
    expect(JSON.parse(put!.body!)).toEqual({ from: { mode: 'domain', domain: 'billing.example.com', keep_origin: false, only_unauthorized: true } });
});

test('the From address form rejects an address on an unverified domain', async ({ page }) => {
    await openSignedIn(page, `/apps/${APP_ID}/smtp`);
    await page.getByTestId('smtp-mode-address').check();
    await page.locator('#smtp-from-address').fill('no-reply@elsewhere.example.com');
    await page.locator('#smtp-from-address').blur();
    await expect(page.locator('#smtp-from-address-hint')).toContainText('verified sending domain');
    await expect(page.getByTestId('smtp-save')).toBeDisabled();
});

test('a message sent over SMTP shows its credential and original From', async ({ page }) => {
    await openSignedIn(page, `/apps/${APP_ID}/messages/${MSG_OLD}`);
    await expect(page.locator('[data-fact="smtp"]')).toContainText('Office printer');
    await expect(page.locator('[data-fact="original-from"]')).toContainText('printer@office.example.net');
});
