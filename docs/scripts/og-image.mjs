// Render docs/og/og-image.html to docs/public/og-image.png (1200x630).
//
// The PNG is committed so the site build never needs a browser; run this only
// after editing the template: `npm run og-image` from docs/. It borrows
// Playwright from ../ui (already a dev dependency there), so run `npm ci` in
// ui/ and `npx playwright install chromium` first if needed.
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";
import { dirname, resolve } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const source = resolve(here, "../og/og-image.html");
const target = resolve(here, "../public/og-image.png");

let chromium;
try {
  ({ chromium } = createRequire(resolve(here, "../../ui/package.json"))("@playwright/test"));
} catch {
  console.error("og-image: @playwright/test not found; run `npm ci` in ui/ first.");
  process.exit(1);
}

const browser = await chromium.launch();
try {
  const page = await browser.newPage({ viewport: { width: 1200, height: 630 }, deviceScaleFactor: 1 });
  await page.goto(pathToFileURL(source).href);
  await page.screenshot({ path: target, clip: { x: 0, y: 0, width: 1200, height: 630 } });
  console.log(`og-image: wrote ${target}`);
} finally {
  await browser.close();
}
