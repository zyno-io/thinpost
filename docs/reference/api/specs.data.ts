import { loadSpecs } from "../../scripts/openapi.mjs";

// Both OpenAPI contracts, read from api/ at build time and bundled so the app
// contract is self-contained. The Reference pages render them.
export default {
  watch: ["../../../api/admin/openapi.yaml", "../../../api/app/openapi.yaml"],
  load() {
    return loadSpecs();
  },
};
