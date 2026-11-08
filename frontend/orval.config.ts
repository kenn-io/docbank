import { defineConfig } from "orval";

export default defineConfig({
  docbank: {
    input: {
      target: "../internal/api/openapi.yaml",
      override: {
        transformer: (spec) => {
          // Huma's wildcard encoding is valid on the wire. Orval gives it
          // precedence over format: binary and otherwise admits string files.
          const binaryUploads = new Set([
            "submitMediaSource",
            "importMediaArtifact",
            "uploadFile",
            "uploadPushFile",
          ]);
          for (const path of Object.values(spec.paths ?? {})) {
            const operation = path?.post;
            const body = operation?.requestBody;
            if (!operation?.operationId || !binaryUploads.has(operation.operationId) || !body || !("content" in body)) continue;
            const multipart = body.content["multipart/form-data"];
            const schema = multipart?.schema;
            if (!schema || !("properties" in schema)) continue;
            const file = schema.properties?.file;
            const encoding = multipart.encoding?.file;
            if (file && "format" in file && file.format === "binary" && encoding?.contentType === "*/*") {
              encoding.contentType = "application/octet-stream";
            }
          }
          return spec;
        },
      },
      filters: { mode: "exclude", tags: ["Photo events"], includeUnreferencedSchemas: true },
    },
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
            operation.headers.schema.model = operation.headers.schema.model.replace(
              /: number/g,
              ": string",
            );
          }
          if ((operation.operationName === "uploadFile" || operation.operationName === "uploadPushFile") && operation.body.formData) {
            const body = `${operation.operationName}Body`;
            const original = operation.body.formData;
            operation.body.formData = original.replace(
              /formData\.append\(`file`,[^;]+;/u,
              `formData.append(\`file\`, ${body}.file, params.name);`,
            );
            if (original === operation.body.formData) throw new Error("Binary upload filename transformer no longer matches generated form data");
          }
          return operation;
        },
        fetch: { includeHttpResponseReturnType: false, arrayFormat: "repeat" },
        mutator: { path: "src/api-transport.ts", name: "sessionJSON" },
        operations: {
          shutdownDaemon: { mutator: { path: "src/api-transport.ts", name: "sessionEmpty" } },
          readPhotoPreview: {
            mutator: { path: "src/api-transport.ts", name: "sessionPhotoPreview", inferred: true },
            requestOptions: { headers: { Accept: "image/jpeg" } },
          },
          readPageImage: {
            mutator: { path: "src/api-transport.ts", name: "sessionResponse", inferred: true },
            requestOptions: { headers: { Accept: "image/png" } },
          },
          getExportJobEvents: {
            mutator: { path: "src/api-transport.ts", name: "sessionResponse", inferred: true },
            requestOptions: { headers: { Accept: "application/x-ndjson" } },
          },
          mailboxEvents: {
            mutator: { path: "src/api-transport.ts", name: "sessionResponse", inferred: true },
            requestOptions: { headers: { Accept: "application/x-ndjson" } },
          },
          vaultChanges: {
            mutator: { path: "src/api-transport.ts", name: "sessionResponse", inferred: true },
            requestOptions: { headers: { Accept: "application/x-ndjson" } },
          },
          submitMediaSource: {
            formData: { path: "src/media-form-data.ts", name: "mediaFormData" },
          },
          importMediaArtifact: {
            formData: { path: "src/media-form-data.ts", name: "mediaFormData" },
          },
          ...Object.fromEntries(
            [
              "renamePerson",
              "mergePerson",
              "createExportSource",
              "sealExportSource",
              "createExportPlan",
              "getExportEmailPDFRecipes",
              "getExportOutputProblems",
              "getExportAttachmentPublications",
              "getExportPlanPreview",
              "createExportJob",
              "getExportJob",
              "downloadExportArchive",
              "downloadTermReportcsv",
              "downloadTermReportbundle",
              "startDocumentProcessing",
              "getDocumentRendition",
              "streamBackupSnapshotRestore",
              "streamBackupSnapshotCreation",
              "streamBackupRepositoryVerification",
              "runDerivativePurge",
              "streamIngest",
              "getNodeContent",
              "getContentVersionBytes",
              "getEmailPart",
              "downloadEmailPDF",
              "readDocumentRenditionBySelector",
              "readRenditionText",
              "readRedactionPreviewPage",
              "readRedactionPreviewText",
              "downloadRedactionPreviewBundle",
              "getSavedQuery",
              "createSavedQuery",
              "updateSavedQuery",
              "deleteSavedQuery",
              "getCollectionLabel",
              "setCollectionLabel",
              "prepareWebDownload",
              "createPhotoAlbum",
              "updatePhotoAlbum",
              "deletePhotoAlbum",
              "duplicatePhotoAlbum",
              "setPhotoAlbumCover",
              "addPhotoAlbumMembers",
              "removePhotoAlbumMembers",
              "createWorkspaceQuery",
              "readWorkspaceQueryPage",
              "getPhotoTimeline",
            ].map((operation) => [
              operation,
              {
                mutator: { path: "src/api-transport.ts", name: "sessionResponse", inferred: true },
              },
            ]),
          ),
        },
      },
    },
  },
});
