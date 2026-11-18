// Prepares the OpenAPI contracts for the Reference pages.
//
// `loadSpecs()` returns both contracts as plain objects. The app contract
// references schemas in the admin contract with a relative file path, which
// breaks when the file is served on its own, so the app contract is bundled:
// the schemas it needs are copied in and the references become local.
//
// Run as a script (the `prebuild` step of the docs build) it also copies the
// contracts into docs/public/openapi/ as YAML and JSON for download. That
// directory is generated and is not committed.
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { parse, stringify } from "yaml";

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = resolve(here, "..", "..");
const adminRef = "../admin/openapi.yaml";

function readSpec(name) {
  return parse(readFileSync(resolve(repoRoot, "api", name, "openapi.yaml"), "utf8"));
}

function schemaName(pointer) {
  const match = /^#\/components\/schemas\/([^/]+)$/.exec(pointer);
  if (!match) throw new Error(`unsupported cross-document reference ${pointer}`);
  return match[1];
}

// Copy every admin schema the app contract reaches, following references
// between the copied schemas, and make all references local.
function bundleApp(app, admin) {
  const copied = new Set();
  const queue = [];
  const localize = (node) => {
    if (Array.isArray(node)) return node.forEach(localize);
    if (node === null || typeof node !== "object") return;
    for (const [key, value] of Object.entries(node)) {
      if (key === "$ref" && typeof value === "string" && value.startsWith(adminRef)) {
        const pointer = value.slice(adminRef.length);
        node[key] = pointer;
        queue.push(schemaName(pointer));
      } else {
        localize(value);
      }
    }
  };
  localize(app);
  app.components.schemas ??= {};
  while (queue.length > 0) {
    const name = queue.pop();
    if (copied.has(name)) continue;
    copied.add(name);
    const schema = admin.components?.schemas?.[name];
    if (!schema) throw new Error(`admin contract has no schema ${name}`);
    if (app.components.schemas[name]) throw new Error(`schema ${name} exists in both contracts`);
    app.components.schemas[name] = structuredClone(schema);
    const refs = [];
    (function find(node) {
      if (Array.isArray(node)) return node.forEach(find);
      if (node === null || typeof node !== "object") return;
      for (const [key, value] of Object.entries(node)) {
        if (key === "$ref" && typeof value === "string" && value.startsWith("#/")) refs.push(schemaName(value));
        else find(value);
      }
    })(schema);
    queue.push(...refs);
  }
  return app;
}

export function loadSpecs() {
  const admin = readSpec("admin");
  return { admin, app: bundleApp(readSpec("app"), admin) };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const out = resolve(here, "..", "public", "openapi");
  mkdirSync(out, { recursive: true });
  for (const [name, spec] of Object.entries(loadSpecs())) {
    writeFileSync(resolve(out, `${name}.yaml`), stringify(spec, { lineWidth: 0 }));
    writeFileSync(resolve(out, `${name}.json`), JSON.stringify(spec, null, 2) + "\n");
  }
}
