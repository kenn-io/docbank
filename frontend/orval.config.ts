import { defineConfig } from "orval";

export default defineConfig({
  docbank: {
    input: "../openapi.yaml",
    output: {
      target: "src/generated/docbank.ts",
      client: "fetch",
      mode: "single",
      override: {
        fetch: { includeHttpResponseReturnType: false },
      },
    },
  },
});
