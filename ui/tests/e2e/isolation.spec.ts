// Iframe isolation and CSP parity. Runs against the production build with the production console CSP
// injected (see fixtures/console.ts). Mocked API and identity provider responses are test-only.
import { expect, test } from '@playwright/test';

import {
    APP_ID,
    injectConsoleCsp,
    installMocks,
    MSG_PLAIN,
    openSignedIn,
    PREVIEW_VISIBLE_TEXT,
    recordRequests,
    remoteHits,
    REMOTE_HOSTS
} from './fixtures/console';

test.beforeEach(async ({ page }) => {
    await injectConsoleCsp(page);
    await installMocks(page);
});

test('the preview renders under the production CSP and makes no remote or relative requests', async ({ page }) => {
    const requests = recordRequests(page);
    const dialogs: string[] = [];
    page.on('dialog', dialog => {
        dialogs.push(dialog.message());
        void dialog.dismiss();
    });
    const blockedByCsp = new Set<string>();
    page.on('requestfailed', request => {
        if (/csp/i.test(request.failure()?.errorText ?? '')) blockedByCsp.add(request.url());
    });
    const violations: string[] = [];
    page.on('console', msg => {
        if (msg.text().includes('Content Security Policy') && msg.text().includes('frame')) violations.push(msg.text());
    });

    await openSignedIn(page, `/apps/${APP_ID}/messages/${MSG_PLAIN}`);
    const frame = page.frameLocator('iframe[sandbox]');
    // frame-src 'none' must not block about:srcdoc.
    await expect(frame.getByText(PREVIEW_VISIBLE_TEXT)).toBeVisible();
    await expect(page.locator('.blocked-note')).toContainText(/external resources? blocked/);
    // Inline cid image resolved from an attachment fetch, rendered from a data: URL.
    await expect(frame.locator('img[alt="logo"]')).toHaveAttribute('src', /^data:image\/png;base64,/);
    await page.waitForTimeout(1500);

    // Requests that reached the network. (Chromium still reports CSP-blocked subresources as requests that fail with errorText "csp";
    // those never reach the network layer, so they are checked separately below.)
    expect(remoteHits).toEqual([]);
    const attempted = requests.filter(r => REMOTE_HOSTS.some(host => r.url.includes(host)));
    for (const r of attempted) {
        expect(blockedByCsp.has(r.url), `${r.url} was attempted and not blocked by CSP`).toBe(true);
    }
    const relative = requests.filter(r => new URL(r.url).pathname.startsWith('/t/'));
    expect(relative.map(r => r.url)).toEqual([]);
    expect(dialogs).toEqual([]);
    expect(violations).toEqual([]);
    // Nothing from the email reached the console DOM.
    expect(await page.locator('main').evaluate(el => el.querySelectorAll('script, img[src*="tracker"], cid-holder').length)).toBe(0);
});

test('meta refresh and links inside the preview never navigate the console', async ({ page }) => {
    const requests = recordRequests(page);
    await openSignedIn(page, `/apps/${APP_ID}/messages/${MSG_PLAIN}`);
    const frame = page.frameLocator('iframe[sandbox]');
    await expect(frame.getByText(PREVIEW_VISIBLE_TEXT)).toBeVisible();
    const url = page.url();
    const popups: string[] = [];
    page.on('popup', popup => popups.push(popup.url()));

    // Links have no href: clicking does nothing.
    await frame.getByText('CLICK THIS LINK').click();
    await page.waitForTimeout(1000);
    expect(page.url()).toBe(url);
    expect(popups).toEqual([]);
    expect(requests.some(r => r.url.includes('beacon.test'))).toBe(false);
    await expect(frame.getByText(PREVIEW_VISIBLE_TEXT)).toBeVisible();
});

test('the iframe is sandboxed with no scripts and an opaque origin', async ({ page }) => {
    await openSignedIn(page, `/apps/${APP_ID}/messages/${MSG_PLAIN}`);
    const iframe = page.locator('iframe[sandbox]');
    await expect(iframe).toHaveAttribute('sandbox', '');
    await expect(iframe).toHaveAttribute('referrerpolicy', 'no-referrer');
    const srcdoc = (await iframe.getAttribute('srcdoc'))!;
    expect(srcdoc).toContain("default-src 'none'");
    expect(srcdoc).not.toMatch(/<script|onerror=|<meta http-equiv="refresh"|href="https?:/i);
});

test('an attachment download is a download event, not a navigation', async ({ page }) => {
    await openSignedIn(page, `/apps/${APP_ID}/messages/${MSG_PLAIN}`);
    await expect(page.getByRole('heading', { name: /Attachments/ })).toBeVisible();
    const url = page.url();
    const [download] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: /Download.*report/ }).click()]);
    expect(download.suggestedFilename()).not.toMatch(/[\\/:*?"]/);
    expect(download.suggestedFilename()).toContain('Q4');
    expect(page.url()).toBe(url);
    await expect(page.locator('iframe[sandbox]')).toHaveCount(1);
});
