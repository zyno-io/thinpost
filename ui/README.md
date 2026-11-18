# Thinpost operator console

The operator console is a Vue 3 and TypeScript single-page application built on `@zyno-io/vue-foundation`. It signs operators in with OpenID Connect and talks to the Thinpost admin API on the same origin. The console styles are part of Thinpost and are MIT-licensed like the rest of the repository.

Documentation:

- What the console does: <https://zyno-io.github.io/thinpost/guide/console>
- Developing it, including the generated client rules and browser checks: <https://zyno-io.github.io/thinpost/contributing/console-dev>

## Development

Node must match [`.node-version`](.node-version).

```sh
npm ci --no-audit --no-fund
npm run gen        # generate the API client from ../api/admin/openapi.yaml
npm run dev        # dev server on http://localhost:7935; set THINPOST_API_URL to proxy /api
npm test           # generates the client, then runs the unit tests
npm run build      # typecheck, then build into dist/
npm run test:e2e   # development-only browser checks (Playwright); not part of CI
```

The API client in `src/openapi-client-generated/` is generated from [`../api/admin/openapi.yaml`](../api/admin/openapi.yaml) and is not committed. Do not edit it by hand.

The control process serves the built console from `THINPOST_UI_DIR`, and the control container image builds it from the checked-in OpenAPI contract.
