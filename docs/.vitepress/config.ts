import { defineConfig } from "vitepress";
import { withMermaid } from "vitepress-plugin-mermaid";

const repo = "https://github.com/zyno-io/thinpost";

export default withMermaid(
  defineConfig({
    title: "Thinpost",
    description:
      "Self-hosted transactional email: Postmark, Resend, SendGrid and Mailgun compatible APIs, app-scoped keys, webhooks and a split control plane and SMTP edge.",
    base: "/thinpost/",
    lang: "en-US",
    cleanUrls: true,
    lastUpdated: true,
    // Dead-link checking stays ON: a broken internal link fails the build.
    ignoreDeadLinks: false,
    // README.md describes the docs directory itself, not a site page.
    srcExclude: ["README.md"],
    head: [["meta", { name: "theme-color", content: "#2f5d8a" }]],
    themeConfig: {
      nav: [
        { text: "Guide", link: "/guide/what-is-thinpost", activeMatch: "/guide/" },
        { text: "Deploy", link: "/deploy/", activeMatch: "/deploy/" },
        { text: "Reference", link: "/reference/configuration", activeMatch: "/reference/" },
        { text: "Operations", link: "/operations/monitoring", activeMatch: "/operations/" },
        { text: "Security", link: "/security/threat-model", activeMatch: "/security/" },
        { text: "Contributing", link: "/contributing/", activeMatch: "/contributing/" },
      ],
      sidebar: {
        "/guide/": [
          {
            text: "Introduction",
            items: [
              { text: "What is Thinpost", link: "/guide/what-is-thinpost" },
              { text: "Concepts", link: "/guide/concepts" },
              { text: "Architecture", link: "/guide/architecture" },
            ],
          },
          {
            text: "Get started",
            items: [
              { text: "Quickstart", link: "/guide/quickstart" },
              { text: "Sending with SDKs", link: "/guide/sending-with-sdks" },
            ],
          },
          {
            text: "Features",
            items: [
              { text: "Sender domains", link: "/guide/domains" },
              { text: "Sending IPs", link: "/guide/sending-ips" },
              { text: "Webhooks", link: "/guide/webhooks" },
              { text: "Open and click tracking", link: "/guide/tracking" },
              { text: "Suppressions", link: "/guide/suppressions" },
              { text: "Inbound mail", link: "/guide/inbound" },
              { text: "Operator console", link: "/guide/console" },
            ],
          },
        ],
        "/deploy/": [
          {
            text: "Deploy",
            items: [
              { text: "Topology and prerequisites", link: "/deploy/" },
              { text: "Database", link: "/deploy/database" },
              { text: "Object storage", link: "/deploy/object-storage" },
              { text: "Operator authentication", link: "/deploy/operator-auth" },
              { text: "Helm: control", link: "/deploy/helm-control" },
              { text: "Helm: Edge", link: "/deploy/helm-edge" },
              { text: "Control PKI", link: "/deploy/control-pki" },
              { text: "Direct public networking", link: "/deploy/direct-public-networking" },
              { text: "Upgrades", link: "/deploy/upgrades" },
            ],
          },
        ],
        "/reference/": [
          {
            text: "Configuration",
            items: [
              { text: "Environment variables", link: "/reference/configuration" },
              { text: "Helm values", link: "/reference/helm-values" },
              { text: "Command line", link: "/reference/cli" },
            ],
          },
          {
            text: "HTTP API",
            items: [
              { text: "Admin API", link: "/reference/api/admin" },
              { text: "App API", link: "/reference/api/app" },
              { text: "OpenAPI contracts", link: "/reference/openapi" },
            ],
          },
          {
            text: "Provider compatibility",
            items: [
              { text: "Overview and matrix", link: "/reference/compatibility/" },
              { text: "Postmark", link: "/reference/compatibility/postmark" },
              { text: "Resend", link: "/reference/compatibility/resend" },
              { text: "SendGrid", link: "/reference/compatibility/sendgrid" },
              { text: "Mailgun", link: "/reference/compatibility/mailgun" },
            ],
          },
          {
            text: "Protocols and formats",
            items: [
              { text: "Webhook formats", link: "/reference/webhook-formats" },
              { text: "Control protocol", link: "/reference/control-protocol" },
            ],
          },
        ],
        "/operations/": [
          {
            text: "Operations",
            items: [
              { text: "Monitoring and health", link: "/operations/monitoring" },
              { text: "Recovery and the admission gate", link: "/operations/recovery" },
              { text: "Key and certificate rotation", link: "/operations/rotation" },
              { text: "Maintenance commands", link: "/operations/maintenance" },
              { text: "Releasing", link: "/operations/releasing" },
            ],
          },
        ],
        "/security/": [
          {
            text: "Security",
            items: [
              { text: "Threat model", link: "/security/threat-model" },
              { text: "Data handling", link: "/security/data-handling" },
              { text: "Reporting vulnerabilities", link: "/security/reporting" },
            ],
          },
        ],
        "/contributing/": [
          {
            text: "Contributing",
            items: [
              { text: "Getting started", link: "/contributing/" },
              { text: "Repository layout", link: "/contributing/layout" },
              { text: "Testing", link: "/contributing/testing" },
              { text: "Console development", link: "/contributing/console-dev" },
              { text: "Generated code", link: "/contributing/generated-code" },
              { text: "Docs site", link: "/contributing/docs-site" },
            ],
          },
        ],
      },
      socialLinks: [{ icon: "github", link: repo }],
      editLink: {
        pattern: `${repo}/edit/main/docs/:path`,
        text: "Edit this page on GitHub",
      },
      search: { provider: "local" },
      footer: {
        message: "MIT licensed.",
      },
      outline: [2, 3],
    },
    vite: {
      ssr: { noExternal: ["vitepress-openapi"] },
    },
    mermaid: {},
    mermaidPlugin: { class: "mermaid" },
  }),
);
