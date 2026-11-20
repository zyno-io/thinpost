// Dev-only checks against the Vite dev server with fabricated API responses (see fixtures/console.ts).
import { expect, test, type Page } from '@playwright/test';

import { APP_ID, installMocks, MSG_EP, MSG_PLAIN, openSignedIn, OTHER_APP_ID, recordRequests, WEBHOOK_SECRET } from './fixtures/console';

test.beforeEach(async ({ page }) => {
    await installMocks(page);
});

test('refresh keeps the session', async ({ page }) => {
    await openSignedIn(page, `/apps/${APP_ID}/messages`);
    await page.reload();
    await expect(page.locator('main#main-content')).toBeVisible();
    await expect(page).toHaveURL(new RegExp(`/apps/${APP_ID}/messages$`));
    await expect(page.getByRole('button', { name: /sign in/i })).toHaveCount(0);
});

test('theme menu supports keyboard selection, persistence and system preference', async ({ page }) => {
    await page.emulateMedia({ colorScheme: 'light' });
    await page.goto('/');
    const trigger = page.getByRole('button', { name: 'Theme', exact: true });
    await trigger.press('ArrowDown');
    await expect(page.getByRole('menuitemradio', { name: 'System' })).toBeFocused();
    await page.getByRole('menu').press('End');
    await expect(page.getByRole('menuitemradio', { name: 'Dark' })).toBeFocused();
    await page.getByRole('menuitemradio', { name: 'Dark' }).press('Enter');
    await expect(page.locator('html')).toHaveClass('dark');
    await expect(trigger).toBeFocused();
    await page.reload();
    await expect(page.locator('html')).toHaveClass('dark');
    await trigger.click();
    await page.getByRole('menuitemradio', { name: 'System' }).click();
    await expect(page.locator('html')).not.toHaveClass('dark');
    await page.emulateMedia({ colorScheme: 'dark' });
    await expect(page.locator('html')).toHaveClass('dark');
});

test('app switcher menu supports keyboard navigation and restores focus on Escape', async ({ page }) => {
    await openSignedIn(page, `/apps/${APP_ID}/messages`);
    const trigger = page.getByRole('button', { name: /Switch application/ });
    await trigger.press('ArrowDown');
    await expect(page.getByRole('menuitemradio', { name: /Billing/ })).toBeFocused();
    await page.getByRole('menu').press('ArrowDown');
    await expect(page.getByRole('menuitemradio', { name: /Notifications/ })).toBeFocused();
    await page.getByRole('menu').press('Escape');
    await expect(trigger).toBeFocused();
    await expect(trigger).toHaveAttribute('aria-expanded', 'false');
    await trigger.click();
    await page.getByRole('menuitemradio', { name: /Notifications/ }).click();
    await expect(page).toHaveURL(new RegExp(`/apps/${OTHER_APP_ID}/messages$`));
});

test('the application header stays mounted across section tabs', async ({ page }) => {
    await openSignedIn(page, `/apps/${APP_ID}`);
    await expect(page.getByRole('heading', { name: 'Billing', level: 1 })).toBeVisible();
    await page.locator('.app-header').evaluate(el => el.setAttribute('data-persist-marker', '1'));
    const tabs = page.getByRole('navigation', { name: 'Application sections' });
    for (const name of ['Sending domains', 'Messages', 'Webhooks', 'Suppressions', 'API keys']) {
        await tabs.getByRole('link', { name }).click();
        await expect(page.locator('.app-header')).toHaveAttribute('data-persist-marker', '1');
        const crumbs = await page.locator('.crumbs').innerText();
        expect(crumbs.replace(/Applications/g, '')).not.toMatch(/\bApplication\b/);
    }
});

test('search keeps terms out of the address bar and sends them only in POST bodies', async ({ page }) => {
    const requests = recordRequests(page);
    await openSignedIn(page, `/apps/${APP_ID}/messages`);
    const startUrl = page.url();
    await expect(page.getByRole('row')).toHaveCount(5);

    const input = page.getByRole('searchbox', { name: 'Search messages' });
    await input.fill('zzsecretterm');
    await input.press('Enter');
    await expect(page.getByText('No matches')).toBeVisible();
    expect(page.url()).toBe(startUrl);

    // Domain shortcut from a list cell.
    await input.fill('');
    await input.press('Enter');
    await page.getByRole('button', { name: 'Search messages from sender domain billing.example.com' }).click();
    await expect(page.getByRole('row')).toHaveCount(2);
    await expect(page.getByLabel('Search field')).toHaveValue('sender_domain');
    expect(page.url()).toBe(startUrl);

    const searches = requests.filter(r => r.url.includes('/messages/search'));
    expect(searches.length).toBeGreaterThan(0);
    for (const r of searches) {
        expect(r.method).toBe('POST');
        expect(r.url).not.toMatch(/[?]/);
    }
    expect(searches.some(r => r.body?.includes('zzsecretterm'))).toBe(true);
    for (const r of requests) expect(r.url).not.toContain('zzsecretterm');
    expect(requests.filter(r => r.method === 'GET' && r.url.includes('/messages') && /[?&](q|field)=/.test(r.url))).toHaveLength(0);
});

test('Enhanced Protection subjects are masked until revealed', async ({ page }) => {
    await openSignedIn(page, `/apps/${APP_ID}/messages`);
    await expect(page.locator('.protected-subject')).toBeVisible();
    await expect(page.getByText('Confidential: test results')).toHaveCount(0);
    await page.getByRole('button', { name: /Reveal/ }).click();
    const dialog = page.locator('.vf-modal-wrap.vf-alert');
    await expect(dialog).toContainText('audit log');
    await dialog.getByRole('button', { name: 'Confirm' }).click();
    await expect(page.getByText('Confidential: test results')).toBeVisible();
});

test('non-EP content auto-loads without a dialog and shows the rendered preview', async ({ page }) => {
    await openSignedIn(page, `/apps/${APP_ID}/messages/${MSG_PLAIN}`);
    await expect(page.locator('iframe[sandbox]')).toHaveCount(1);
    await expect(page.locator('.vf-modal-wrap.vf-alert')).toHaveCount(0);
    await expect(page.locator('main')).not.toContainText(/audit/i);
});

test('EP detail requires an explicit, confirmed content load', async ({ page }) => {
    await openSignedIn(page, `/apps/${APP_ID}/messages/${MSG_EP}`);
    await expect(page.locator('iframe[sandbox]')).toHaveCount(0);
    await page.getByRole('button', { name: 'Show content' }).click();
    await expect(page.locator('.vf-modal-wrap.vf-alert')).toContainText('audit log');
});

test('adding a webhook endpoint shows the signing secret once and keeps it out of storage and the URL', async ({ page }) => {
    await openSignedIn(page, `/apps/${APP_ID}/webhooks`);
    await expect(page.getByText('Endpoints are approved by the operator')).toHaveCount(0);
    await expect(page.locator('main')).toContainText('https://hooks.example.com/thinpost');

    await page.getByRole('button', { name: 'Add endpoint' }).click();
    await page.getByLabel('Endpoint URL').fill('http://hooks.example.com/x');
    await page.getByRole('button', { name: 'Add endpoint' }).last().click();
    await expect(page.getByRole('dialog')).toContainText('Use an https:// URL.');

    await page.getByLabel('Endpoint URL').fill('https://hooks.example.com/new?token=1');
    await page.getByRole('button', { name: 'Add endpoint' }).last().click();
    const secret = page.getByRole('alertdialog');
    await expect(secret).toContainText('Signing secret issued');
    await expect(secret.getByLabel('Secret')).toHaveValue(WEBHOOK_SECRET);
    expect(page.url()).not.toContain(WEBHOOK_SECRET);
    expect(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))).not.toContain(WEBHOOK_SECRET);

    await secret.getByRole('button', { name: 'Copy' }).click().catch(() => undefined);
    await secret.getByRole('button', { name: 'Done' }).click();
    // Closing without copying asks first; confirm it so the secret is dropped either way.
    const confirm = page.locator('.vf-modal-wrap.vf-alert');
    if (await confirm.isVisible()) await confirm.getByRole('button').last().click();
    await expect(page.locator('body')).not.toContainText(WEBHOOK_SECRET);
});

const SCREENS: Array<{ name: string; path: string; ready: RegExp | string }> = [
    { name: 'apps', path: '/apps', ready: 'Applications' },
    { name: 'keys', path: `/apps/${APP_ID}`, ready: 'API keys' },
    { name: 'domains', path: `/apps/${APP_ID}/domains`, ready: 'Sending domains' },
    { name: 'messages', path: `/apps/${APP_ID}/messages`, ready: 'Messages' },
    { name: 'message', path: `/apps/${APP_ID}/messages/${MSG_PLAIN}`, ready: 'Deliveries' },
    { name: 'message-ep', path: `/apps/${APP_ID}/messages/${MSG_EP}`, ready: 'Deliveries' },
    { name: 'webhooks', path: `/apps/${APP_ID}/webhooks`, ready: /webhook/i },
    { name: 'tracking', path: `/apps/${APP_ID}/tracking`, ready: 'Daily event budget (UTC)' },
    { name: 'operations', path: '/operations', ready: 'Operations' },
    { name: 'not-found', path: '/no/such/page', ready: 'Page not found' }
];

async function hasHorizontalOverflow(page: Page) {
    return page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth);
}

test('no screen or modal overflows horizontally at 360px', async ({ page }) => {
    await page.setViewportSize({ width: 360, height: 800 });
    for (const screen of SCREENS) {
        await openSignedIn(page, screen.path);
        await expect(page.getByRole('heading', { name: screen.ready }).first()).toBeVisible();
        expect(await hasHorizontalOverflow(page), `${screen.name} overflows`).toBe(false);
    }

    await openSignedIn(page, `/apps/${APP_ID}`);
    await page.getByRole('button', { name: 'Create key' }).click();
    const modal = page.locator('.vf-modal');
    await expect(modal).toBeVisible();
    const box = (await modal.boundingBox())!;
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(360);
    await page.getByRole('button', { name: 'Cancel' }).click();

    await openSignedIn(page, `/apps/${APP_ID}/webhooks`);
    await page.getByRole('button', { name: 'Add endpoint' }).click();
    const endpointModal = page.locator('.vf-modal');
    await expect(endpointModal).toBeVisible();
    const endpointBox = (await endpointModal.boundingBox())!;
    expect(endpointBox.x).toBeGreaterThanOrEqual(0);
    expect(endpointBox.x + endpointBox.width).toBeLessThanOrEqual(360);
    await page.getByRole('button', { name: 'Cancel' }).click();

    await openSignedIn(page, `/apps/${APP_ID}/messages/${MSG_PLAIN}`);
    await page.getByRole('button', { name: /^Headers/ }).first().click();
    await expect(page.getByRole('dialog')).toContainText('Received');
    const headers = (await page.locator('.vf-modal').boundingBox())!;
    expect(headers.x).toBeGreaterThanOrEqual(0);
    expect(headers.x + headers.width).toBeLessThanOrEqual(360);
});

for (const colorScheme of ['light', 'dark'] as const) {
    test(`tags remain readable in ${colorScheme} mode`, async ({ page }) => {
        await page.emulateMedia({ colorScheme });
        await openSignedIn(page, `/apps/${APP_ID}/messages`);
        await expect(page.locator('.tag').first()).toBeVisible();
        const ratios = await page.locator('.tag').evaluateAll(elements => {
            const rgb = (value: string) => value.match(/[\d.]+/g)!.map(Number);
            const luminance = (color: number[]) =>
                color.slice(0, 3).reduce((sum, channel, i) => {
                    const n = channel / 255;
                    return sum + (n <= 0.04045 ? n / 12.92 : ((n + 0.055) / 1.055) ** 2.4) * [0.2126, 0.7152, 0.0722][i]!;
                }, 0);
            return elements.map(element => {
                let background = [255, 255, 255];
                const ancestors: Element[] = [];
                for (let node: Element | null = element; node; node = node.parentElement) ancestors.unshift(node);
                for (const node of ancestors) {
                    const color = rgb(getComputedStyle(node).backgroundColor);
                    const alpha = color[3] ?? 1;
                    background = background.map((channel, i) => color[i]! * alpha + channel * (1 - alpha));
                }
                const foreground = luminance(rgb(getComputedStyle(element).color));
                const backdrop = luminance(background);
                return (Math.max(foreground, backdrop) + 0.05) / (Math.min(foreground, backdrop) + 0.05);
            });
        });
        expect(ratios.length).toBeGreaterThan(0);
        for (const ratio of ratios) expect(ratio).toBeGreaterThanOrEqual(4.5);
    });
}

test('suppression search keeps terms out of the address bar and sends them only in POST bodies', async ({ page }) => {
    const requests = recordRequests(page);
    await openSignedIn(page, `/apps/${APP_ID}/suppressions`);
    const startUrl = page.url();
    await expect(page.getByRole('row')).toHaveCount(4);
    const input = page.getByRole('searchbox', { name: 'Search suppressions' });
    await input.fill('zzsecretterm@');
    await input.press('Enter');
    await expect(page.getByText('No matches')).toBeVisible();
    expect(page.url()).toBe(startUrl);
    expect(requests.some(r => r.url.includes('zzsecretterm'))).toBe(false);
    expect(requests.some(r => r.url.includes('/suppressions/search') && r.method === 'POST' && r.body?.includes('zzsecretterm'))).toBe(true);
});
