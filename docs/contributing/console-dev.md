---
title: Console development
description: How to develop the Thinpost operator console, the rules for the generated API client, and the invariants UI changes must keep.
---

# Console development

The operator console in `ui/` is a Vue 3 and TypeScript single-page application built on `@zyno-io/vue-foundation`. It is a client of the [admin API](/reference/api/admin) and has no backend of its own. The control image builds it and serves it on the same origin as `/api/v1`. See [Operator console](/guide/console) for what it does.

## Workflow

Node must match `ui/.node-version` (26). From `ui/`:

```sh
npm ci --no-audit --no-fund
npm run gen        # generate the API client from ../api/admin/openapi.yaml
npm run dev        # dev server on http://localhost:7935
npm test           # generates the client, then runs the unit tests (Vitest, happy-dom)
npm run typecheck  # generates the client, then runs vue-tsc
npm run build      # typecheck, then build into dist/
```

`npm test`, `npm run typecheck` and `npm run build` regenerate the client first, so they never run against a stale one.

### Against a running control plane

The dev server can forward `/api` to a control plane you run separately:

```sh
THINPOST_API_URL=http://localhost:8080 npm run dev
```

Signing in needs an OIDC provider whose registration allows the dev origin as a redirect URI, `http://localhost:7935/auth/callback`. A dev-server proxy does not register that for you. For a full local stack with a development sign-in service, use the [quickstart](/guide/quickstart).

The built console must be served on the same origin as `/api/v1`, with a fallback to `index.html` for client routes such as `/auth/callback`. `thinpost-control` does that when `THINPOST_UI_DIR` names the built `dist/` directory, and the control image sets it.

## The generated API client

The client in `ui/src/openapi-client-generated/` is generated from `api/admin/openapi.yaml` and is **not committed**.

- Backend HTTP contracts belong in the OpenAPI file. Add or change an operation there first, then run `npm run gen`.
- Never hand-edit or commit generated files, and never invent a frontend-only contract. If the UI needs data the API does not return, change the API.
- Call generated operations by their `operationId`. Keep the OpenAPI descriptions accurate, because they are the API reference.

## Invariants for UI changes

These rules keep the console safe to use with real mail. A change must keep them.

**Browser storage**
- The signed-in OIDC user persists in `localStorage` so refreshes and new tabs stay signed in until the token expires or the operator signs out. The record holds the ID token, access token, scope, expiry and profile claims. A refresh token is removed before writing.
- Nothing else sensitive may be stored. Never put credentials, key secrets, message bodies, subjects, addresses, search terms or tracking links in URLs, browser storage, history state, logs or error telemetry.
- The theme preference, the last-selected app ID and OIDC redirect state may be stored.
- Search terms travel only in `POST` bodies and live in memory.

**Secrets and content**
- Show issued key secrets and webhook signing secrets once, hold them only in component memory, and clear them when the dialog closes. Keep the navigation guards that protect an unfinished display.
- Render stored email HTML only inside a sandboxed, opaque-origin iframe with a sanitized `srcdoc` and a restrictive inner policy. Never insert it into the console's DOM. Remote loads stay blocked, which the console Content-Security-Policy also enforces.
- Enhanced Protection content needs explicit confirmation before it is shown, and the server audits the read.
- Attachments download as opaque blobs under a sanitized file name, and object URLs are revoked.

**Actions and errors**
- Destructive, replay and retry actions need an accurate confirmation, server-side authorization and an audit record.
- An ambiguous mutation response, such as no response, a 5xx or an unexpected success code, requires reloading state. Never retry automatically, because that can duplicate a send or a webhook.
- Keep app scoping, handling for 401, 403, 404, 409 and 503, cancellation, stale-response protection and memory cleanup. A backend or dispatcher outage must stay visible. Data the API cannot provide is shown as unavailable, never as zero.
- Await results into named locals before using them in another operation.
- Never put endpoint URLs or secrets in logs or audit entries.

**Scripts and styles**
- The console's own Content-Security-Policy forbids inline scripts and `eval`, so the theme boot script is the external file `public/theme-boot.js`.
- The console styles are part of Thinpost and MIT-licensed like the rest of the repository.

## Browser checks

Playwright checks run against fabricated API and identity-provider responses installed with `page.route`, defined in `ui/tests/e2e/fixtures/console.ts`. They are development-only and are not part of `npm test` or CI.

```sh
npx playwright install chromium   # once
npm run test:e2e
```

The `dev` project drives the Vite dev server: theme and menu keyboard use, shell persistence, search terms never reaching the URL, no horizontal overflow at 360 px, tag contrast, and a light and dark screenshot smoke test written to `test-results/screenshots`. The `csp-chromium` project serves the production build with the production Content-Security-Policy and checks that the sandboxed preview renders, makes no remote or relative requests and never navigates, and that attachment downloads are downloads.

Set `E2E_ALL_BROWSERS=1`, after `npx playwright install firefox webkit`, to run the isolation checks in Firefox and WebKit too.

These checks are not a substitute for trying a change through the real sign-in flow against a real control plane. Do that for anything that touches authentication, content rendering or an audited action.
