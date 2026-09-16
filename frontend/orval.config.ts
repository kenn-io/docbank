import { defineConfig } from "orval";

export default defineConfig({
  docbank: {
    input: "../openapi.yaml",
    output: {
      target: "src/generated/docbank.ts",
      client: "fetch",
      mode: "single",
      urlEncodeParameters: true,
      override: {
        fetch: { includeHttpResponseReturnType: false, arrayFormat: "repeat" },
        mutator: { path: "src/api-transport.ts", name: "sessionJSON" },
        operations: Object.fromEntries([
          "startDocumentProcessing", "getDocumentRendition",
          "streamBackupSnapshotRestore", "streamBackupSnapshotCreation",
          "streamBackupRepositoryVerification", "runDerivativePurge", "streamIngest",
          "getNodeContent", "getContentVersionBytes", "getEmailPart",
          "readDocumentRenditionBySelector",
          "getSavedQuery", "createSavedQuery", "updateSavedQuery", "deleteSavedQuery",
          "getCollectionLabel", "setCollectionLabel", "prepareWebDownload",
        ].map((operation) => [operation, {
          mutator: { path: "src/api-transport.ts", name: "sessionResponse", inferred: true },
        }])),
      },
    },
  },
});
