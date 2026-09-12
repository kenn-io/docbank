import { defineConfig } from "orval";

export default defineConfig({
  docbank: {
    input: "../openapi.yaml",
    output: {
      target: "src/generated/docbank.ts",
      client: "fetch",
      mode: "single",
      headers: true,
      urlEncodeParameters: true,
      override: {
        splitByContentType: true,
        transformer: (operation) => {
          // Fetch headers are text, including the contract's integer byte count.
          if (operation.headers) {
            operation.headers.schema.model = operation.headers.schema.model.replace(/: number/g, ": string");
          }
          if (operation.operationName === "uploadFile" && operation.body.formData) {
            operation.body.formData = operation.body.formData.replace("uploadFileBody.file);", "uploadFileBody.file, params.name);");
          }
          return operation;
        },
        fetch: { includeHttpResponseReturnType: false, arrayFormat: "repeat" },
        mutator: { path: "src/api-transport.ts", name: "sessionJSON" },
        operations: {
          shutdownDaemon: { mutator: { path: "src/api-transport.ts", name: "sessionEmpty" } },
          readPageImage: {
            mutator: { path: "src/api-transport.ts", name: "sessionResponse", inferred: true },
            requestOptions: { headers: { Accept: "image/png" } },
          },
          getExportJobEvents: {
            mutator: { path: "src/api-transport.ts", name: "sessionResponse", inferred: true },
            requestOptions: { headers: { Accept: "application/x-ndjson" } },
          },
          submitMediaSource: { formData: { path: "src/media-form-data.ts", name: "mediaFormData" } },
          importMediaArtifact: { formData: { path: "src/media-form-data.ts", name: "mediaFormData" } },
          ...Object.fromEntries([
          "createExportSource", "sealExportSource", "createExportPlan",
          "getExportPlanPreview", "createExportJob", "getExportJob", "downloadExportArchive",
          "startDocumentProcessing", "getDocumentRendition",
          "streamBackupSnapshotRestore", "streamBackupSnapshotCreation",
          "streamBackupRepositoryVerification", "runDerivativePurge", "streamIngest",
          "getNodeContent", "getContentVersionBytes", "getEmailPart",
          "readDocumentRenditionBySelector", "readRenditionText",
          "getSavedQuery", "createSavedQuery", "updateSavedQuery", "deleteSavedQuery",
          "getCollectionLabel", "setCollectionLabel", "prepareWebDownload",
          "createWorkspaceQuery", "readWorkspaceQueryPage",
        ].map((operation) => [operation, {
          mutator: { path: "src/api-transport.ts", name: "sessionResponse", inferred: true },
        }])),
        },
      },
    },
  },
});
