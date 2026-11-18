// SEO gate for the docs site.
//
// Sources: every published page needs a `title` (frontmatter, or an H1 as
// VitePress falls back to) and a `description` of 50-160 characters.
// Build output (when .vitepress/dist exists): a sample page must carry the
// canonical link and the Open Graph / Twitter tags, and the sitemap, robots.txt
// and OG image must exist. Run by `postdocs:build`; `--sources` skips the
// build checks, `--root <dir>` points at another docs tree (used for testing).
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { parse } from "yaml";

const MIN = 50;
const MAX = 160;
const SITE = "https://zyno-io.github.io/thinpost/";

const args = process.argv.slice(2);
const sourcesOnly = args.includes("--sources");
const rootIdx = args.indexOf("--root");
const root = resolve(rootIdx >= 0 ? args[rootIdx + 1] : join(dirname(fileURLToPath(import.meta.url)), ".."));
const errors = [];

function* walk(dir) {
  for (const name of readdirSync(dir)) {
    if (["node_modules", ".vitepress", "public", "og", "scripts"].includes(name)) continue;
    const p = join(dir, name);
    if (statSync(p).isDirectory()) yield* walk(p);
    else if (name.endsWith(".md") && !(dir === root && name === "README.md")) yield p;
  }
}

for (const file of walk(root)) {
  const rel = relative(root, file);
  const src = readFileSync(file, "utf8");
  const m = src.match(/^---\r?\n([\s\S]*?)\r?\n---/);
  let fm = {};
  try {
    fm = m ? (parse(m[1]) ?? {}) : {};
  } catch (e) {
    errors.push(`${rel}: invalid frontmatter (${e.message.split("\n")[0]})`);
    continue;
  }
  if (!fm.title && !/^# \S/m.test(src.slice(m ? m[0].length : 0))) errors.push(`${rel}: missing title`);
  const d = typeof fm.description === "string" ? fm.description.trim() : "";
  if (!d) errors.push(`${rel}: missing description`);
  else if (d.length < MIN || d.length > MAX) errors.push(`${rel}: description is ${d.length} characters (want ${MIN}-${MAX})`);
}

const dist = join(root, ".vitepress", "dist");
if (!sourcesOnly && existsSync(dist)) {
  const read = (f) => (existsSync(join(dist, f)) ? readFileSync(join(dist, f), "utf8") : null);
  const need = [
    ['rel="canonical"', "canonical link"],
    ['property="og:type"', "og:type"],
    ['property="og:site_name"', "og:site_name"],
    ['property="og:title"', "og:title"],
    ['property="og:description"', "og:description"],
    ['property="og:url"', "og:url"],
    [`property="og:image" content="${SITE}og-image.png"`, "og:image"],
    ['property="og:image:width" content="1200"', "og:image:width"],
    ['property="og:image:height" content="630"', "og:image:height"],
    ['property="og:image:alt"', "og:image:alt"],
    ['name="twitter:card" content="summary_large_image"', "twitter:card"],
    ['name="twitter:title"', "twitter:title"],
    ['name="twitter:description"', "twitter:description"],
    ['name="twitter:image"', "twitter:image"],
  ];
  for (const [page, url] of [
    ["index.html", SITE],
    ["guide/quickstart.html", `${SITE}guide/quickstart`],
  ]) {
    const html = read(page);
    if (html === null) {
      errors.push(`dist/${page}: not built`);
      continue;
    }
    for (const [needle, label] of need) if (!html.includes(needle)) errors.push(`dist/${page}: missing ${label}`);
    if (!html.includes(`rel="canonical" href="${url}"`)) errors.push(`dist/${page}: canonical is not ${url}`);
  }
  const sitemap = read("sitemap.xml");
  if (!sitemap || !sitemap.includes(`<loc>${SITE}`)) errors.push("dist/sitemap.xml: missing or not under the site URL");
  if (read("robots.txt") === null) errors.push("dist/robots.txt: missing");
  if (!existsSync(join(dist, "og-image.png"))) errors.push("dist/og-image.png: missing");
  if (!existsSync(join(dist, "favicon.svg"))) errors.push("dist/favicon.svg: missing");
}

if (errors.length) {
  console.error(`check-seo: ${errors.length} problem(s)`);
  for (const e of errors) console.error(`  ${e}`);
  process.exit(1);
}
console.log(`check-seo: ok${!sourcesOnly && existsSync(dist) ? " (sources and built output)" : " (sources)"}`);
