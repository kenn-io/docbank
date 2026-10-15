// @vitest-environment node
import { afterEach, expect, it, vi } from "vite-plus/test";
import * as api from "./generated/docbank";

afterEach(() => vi.restoreAllMocks());

it("sends JSON metadata before the media file without a metadata filename", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) => {
    const response = new Response(await (request?.body as Blob).text(), {
      headers: request?.headers,
    });
    const parts = await response.formData();
    expect([...parts.keys()]).toEqual(["metadata", "file"]);
    expect(typeof parts.get("metadata")).toBe("string");
    const file = parts.get("file") as File;
    expect(file.name).toBe("sample.txt");
    expect(await file.text()).toBe("synthetic");
    return Response.json({});
  });
  const metadata: api.MediaArtifactMetadata = {
    filename: "sample.txt",
    media_type: "text/plain",
    byte_length: 9,
    sha256: "a".repeat(64),
    operation_id: "synthetic-operation",
    occurrence_id: "synthetic-occurrence",
    kind: "transcript",
  };
  await api.importMediaArtifact("synthetic-source", { metadata, file: new Blob(["synthetic"]) });
  expect(fetch).toHaveBeenCalledOnce();
});

it("uses the declared filename when uploading a Blob", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({}));
  await api.uploadFile(
    { file: new Blob(["synthetic"]) },
    { parent_id: 1, name: "sample.txt" },
    {
      "X-Docbank-Blob-Hash": "a".repeat(64),
      "X-Docbank-Blob-Size": "9",
    },
  );
  const body = fetch.mock.calls[0][1]?.body as FormData;
  expect((body.get("file") as File).name).toBe("sample.txt");
});

it("preserves a ranged Markdown response and repeats coverage IDs", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response("# synthetic", {
      status: 206,
      headers: { "Content-Type": "text/markdown", "Content-Range": "bytes 0-10/20" },
    }),
  );
  const response = await api.getDocumentRendition("a".repeat(64), undefined, {
    Range: "bytes=0-10",
  });
  expect(response.status).toBe(206);
  expect(response.headers.get("Content-Range")).toBe("bytes 0-10/20");
  expect(await response.text()).toBe("# synthetic");
  expect(new Headers(fetch.mock.calls[0][1]?.headers).get("Range")).toBe("bytes=0-10");
  const url = new URL(
    api.getGetDocumentProcessingCoverageUrl({
      profile: "synthetic",
      vault_uid: "synthetic-vault",
      content_version_id: ["one", "two"],
    }),
    "http://localhost",
  );
  expect(url.searchParams.getAll("content_version_id")).toEqual(["one", "two"]);
});

it.each([
  {
    download: api.downloadTermReportcsv,
    type: "text/csv",
    body: Buffer.from("Term #,Terms,Hits\n1,alpha,1\n"),
  },
  {
    download: api.downloadTermReportbundle,
    type: "application/zip",
    body: Buffer.from([80, 75, 5, 6, ...Array(18).fill(0)]),
  },
])("preserves search-export $type download bytes", async ({ download, type, body }) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(body, { headers: { "Content-Type": type } }),
  );
  const response = await download("a".repeat(48), { headers: { "X-Api-Key": "synthetic-key" } });
  expect(response).toBeInstanceOf(Response);
  expect(Buffer.from(await response.arrayBuffer())).toEqual(body);
});

it("accepts the empty shutdown acknowledgement", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 202 }));
  await expect(
    api.shutdownDaemon({ "X-Docbank-Daemon-Token": "synthetic" }),
  ).resolves.toBeUndefined();
});

it("reads page images as PNG bytes with PNG Accept", async () => {
  const png = Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII=",
    "base64",
  );
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(png, {
      headers: { "Content-Type": "image/png" },
    }),
  );
  const response = await api.readPageImage(
    {
      node_id: 1,
      revision: 2,
      version_id: "11111111-1111-4111-8111-111111111111",
      source_sha256: "a".repeat(64),
      source_size: 100,
      page: 1,
      recipe_sha256: "b".repeat(64),
      frame_sha256: "c".repeat(64),
      image_sha256: "d".repeat(64),
    },
    { session: "synthetic-session" },
  );
  expect(Buffer.from(await response.arrayBuffer())).toEqual(png);
  const headers = new Headers(fetch.mock.calls[0][1]?.headers);
  expect(headers.get("Accept")).toBe("image/png");
});

it("preserves export progress as an NDJSON response", async () => {
  const body =
    '{"delivery":"current_state","job":{"sequence":1}}\n{"delivery":"current_state","job":{"sequence":2}}\n';
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(body, {
      headers: { "Content-Type": "application/x-ndjson" },
    }),
  );
  const response = await api.getExportJobEvents(
    "synthetic-job",
    { after: 1 },
    { session: "synthetic-session" },
  );
  expect(response).toBeInstanceOf(Response);
  expect(await response.text()).toBe(body);
  const [, request] = fetch.mock.calls[0];
  expect(new Headers(request?.headers).get("Accept")).toBe("application/x-ndjson");
});

it("preserves a conditional photo preview's empty 304 response", async () => {
  const etag = '"cached-generation"';
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(null, {
      status: 304,
      headers: { ETag: etag, "Cache-Control": "private, no-cache" },
    }),
  );
  const response = await api.readPhotoPreview(
    "00000000-0000-4000-8000-000000000001",
    "a".repeat(64),
    { "If-None-Match": etag },
    { session: "synthetic-session" },
  );
  expect(response.status).toBe(304);
  expect(response.headers.get("ETag")).toBe(etag);
  expect(await response.text()).toBe("");
  const [url, request] = fetch.mock.calls[0];
  expect(url).toBe(
    `/api/v1/photos/assets/00000000-0000-4000-8000-000000000001/previews/${"a".repeat(64)}`,
  );
  const headers = new Headers(request?.headers);
  expect(headers.get("Accept")).toBe("image/jpeg");
  expect(headers.get("If-None-Match")).toBe(etag);
});

it("keeps 304 outside JSON response contracts", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 304 }));
  await expect(api.listPhotoAssets({ query: {} } as api.PhotoBrowseRequest)).rejects.toMatchObject({
    status: 304,
    message: "HTTP 304",
  });
});
