import DefaultTheme from "vitepress/theme";
import type { Theme } from "vitepress";
import { theme as openapiTheme, useTheme } from "vitepress-openapi/client";
import "vitepress-openapi/dist/style.css";

// vitepress-openapi renders the OpenAPI contracts on the Reference pages. Each
// page passes its own spec (see docs/reference/api/specs.data.ts), so no spec
// is registered globally. The in-page playground is hidden: the APIs are
// served by your own deployment, not by this site.
useTheme({
  operation: { hiddenSlots: ["playground", "branding"], defaultBaseUrl: "https://thinpost.example.com" },
  spec: { groupByTags: false },
});

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    openapiTheme.enhanceApp({ app });
  },
} satisfies Theme;
