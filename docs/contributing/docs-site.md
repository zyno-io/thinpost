---
title: Docs site
description: How to write, preview and build the Thinpost documentation site.
---

# Docs site

This site is [VitePress](https://vitepress.dev) 1.x in `docs/`, a private npm package (`@zyno-io/thinpost-docs`). It is published at `https://zyno-io.github.io/thinpost/`, so its base path is `/thinpost/`.

## Workflow

```sh
cd docs
npm ci
npm run docs:dev       # live preview
npm run docs:build     # production build; fails on dead links
npm run docs:preview   # serve the build output
```

Node must match `ui/.node-version`.

## Writing pages

- Pages are Markdown under `docs/`, grouped as `guide/`, `deploy/`, `reference/`, `operations/`, `security/` and `contributing/`. Add a new page to the sidebar in `docs/.vitepress/config.ts`.
- Start each page with front matter that has a `title` and a one-sentence `description`.
- Link to other pages with site-absolute paths such as `/guide/concepts`. Dead-link checking is on, so a broken internal link fails the build. Links to files outside `docs/`, such as source files, must be full GitHub URLs.
- Put angle-bracket placeholders such as `<namespace>` and Helm `{{ }}` expressions in code spans or code blocks, never bare in prose. Vue otherwise parses them as tags. Use a `::: v-pre` block for template syntax in prose.
- Mermaid diagrams use fenced `mermaid` blocks.
- Write in the present tense. Describe what Thinpost does, not its history. Leave out dates, ticket and pull request numbers, and status that will go stale.
- Use placeholders in examples: `example.com`, `.invalid` names and the RFC 5737 ranges (`192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`). Never add real hosts, addresses, credentials or customer names.
- Reference pages for configuration and Helm values are checked against the code in CI. When you add or change a setting, update them in the same pull request.

## How the site is published

A workflow builds the site on every pull request that touches `docs/`, the API contracts, the chart or configuration code, and fails on errors. On pushes to `main` it builds the site and deploys it to GitHub Pages. Pull requests never deploy.

Build output in `docs/.vitepress/dist` and the cache are git-ignored.
