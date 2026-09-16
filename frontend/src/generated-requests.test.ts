// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import * as api from "./generated/docbank";

afterEach(() => vi.restoreAllMocks());

it("sends reference media as JSON and rejects a conflict", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(
    JSON.stringify({ code: "conflict", detail: "Changed" }), { status: 412 },
  ));
  const body: api.MediaReferenceBody = {
    operation_id: "synthetic-operation", provider_hint: "synthetic", reference_url: "https://example.com/sample.wav", acquire: false,
    occurrence: { filename: "sample.wav", ref: "synthetic", revision: "1", message: {
      raw: "2026-09-01", normalized: "2026-09-01", precision: "day", timezone: "UTC", zone_text: "UTC", fraction_digits: 0,
    } },
  };
  await expect(api.submitMediaSourceWithJson(body)).rejects.toMatchObject({ status: 412, code: "conflict" });
  const [, request] = fetch.mock.calls[0];
  expect(new Headers(request?.headers).get("Content-Type")).toBe("application/json");
  expect(JSON.parse(String(request?.body))).toEqual(body);
});

it("sends JSON metadata before the media file without a metadata filename", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, request) => {
    const response = new Response(await (request?.body as Blob).text(), { headers: request?.headers });
    const parts = await response.formData();
    expect([...parts.keys()]).toEqual(["metadata", "file"]);
    expect(typeof parts.get("metadata")).toBe("string");
    const file = parts.get("file") as File;
    expect(file.name).toBe("sample.txt");
    expect(await file.text()).toBe("synthetic");
    return Response.json({});
  });
  const metadata: api.MediaArtifactMetadata = {
    filename: "sample.txt", media_type: "text/plain", byte_length: 9, sha256: "a".repeat(64),
    operation_id: "synthetic-operation", occurrence_id: "synthetic-occurrence", kind: "transcript",
  };
  await api.importMediaArtifact("synthetic-source", { metadata, file: new Blob(["synthetic"]) });
  expect(fetch).toHaveBeenCalledOnce();
});

it("uses the declared filename when uploading a Blob", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({}));
  await api.uploadFile({ file: new Blob(["synthetic"]) }, { parent_id: 1, name: "sample.txt" }, {
    "X-Docbank-Blob-Hash": "a".repeat(64), "X-Docbank-Blob-Size": "9",
  });
  const body = fetch.mock.calls[0][1]?.body as FormData;
  expect((body.get("file") as File).name).toBe("sample.txt");
});

it("preserves a ranged Markdown response and repeats coverage IDs", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("# synthetic", {
    status: 206, headers: { "Content-Type": "text/markdown", "Content-Range": "bytes 0-10/20" },
  }));
  const response = await api.getDocumentRendition("a".repeat(64), undefined, { Range: "bytes=0-10" });
  expect(response.status).toBe(206);
  expect(response.headers.get("Content-Range")).toBe("bytes 0-10/20");
  expect(await response.text()).toBe("# synthetic");
  expect(new Headers(fetch.mock.calls[0][1]?.headers).get("Range")).toBe("bytes=0-10");
  const url = new URL(api.getGetDocumentProcessingCoverageUrl({ profile: "synthetic", vault_uid: "synthetic-vault", content_version_id: ["one", "two"] }), "http://localhost");
  expect(url.searchParams.getAll("content_version_id")).toEqual(["one", "two"]);
});
