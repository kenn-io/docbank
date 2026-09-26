// @vitest-environment node
import { afterEach, expect, it, vi } from "vitest";
import * as api from "./generated/docbank";
import { streamExportJobEvents } from "./export-events";

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

it.each([
  { download: api.downloadTermReportcsv, type: "text/csv", body: Buffer.from("Term #,Terms,Hits\n1,alpha,1\n") },
  { download: api.downloadTermReportbundle, type: "application/zip", body: Buffer.from([80, 75, 5, 6, ...Array(18).fill(0)]) },
])("preserves search-export $type download bytes", async ({ download, type, body }) => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(body, { headers: { "Content-Type": type } }));
  const response = await download("a".repeat(48), { headers: { "X-Api-Key": "synthetic-key" } });
  expect(response).toBeInstanceOf(Response);
  expect(Buffer.from(await response.arrayBuffer())).toEqual(body);
});

it("sends an exact selected-document scope through the generated report client", async () => {
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(Response.json({}));
  const selected = { node_id: 3, version_id: "00000003-1111-4111-8111-111111111111", sha256: "3".repeat(64) };
  const request: api.Request = {
    version: 2, all_documents: false, selected_documents: [selected], timezone: "UTC", coverage_mode: "strict",
    terms: [{ number: 1, syntax: "simple", expression: "alpha", dates: { start: "2026-09-01", end: "2026-09-30" } }],
  };
  await api.createTermReport(request, { session: "synthetic-session" });
  expect(JSON.parse(String(fetch.mock.calls[0][1]?.body))).toMatchObject({
    version: 2, all_documents: false, selected_documents: [selected],
  });
});

it("accepts the empty shutdown acknowledgement", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(null, { status: 202 }));
  await expect(api.shutdownDaemon({ "X-Docbank-Daemon-Token": "synthetic" })).resolves.toBeUndefined();
});

it("reads page images as PNG bytes through the browser session", async () => {
  const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+ip1sAAAAASUVORK5CYII=", "base64");
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(png, {
    headers: { "Content-Type": "image/png" },
  }));
  const response = await api.readPageImage({
    node_id: 1, revision: 2, version_id: "11111111-1111-4111-8111-111111111111",
    source_sha256: "a".repeat(64), source_size: 100, page: 1,
    recipe_sha256: "b".repeat(64), frame_sha256: "c".repeat(64), image_sha256: "d".repeat(64),
  }, { session: "synthetic-session" });
  expect(Buffer.from(await response.arrayBuffer())).toEqual(png);
  const headers = new Headers(fetch.mock.calls[0][1]?.headers);
  expect(headers.get("Accept")).toBe("image/png");
  expect(headers.get("X-Docbank-Web-Session")).toBe("synthetic-session");
});

it("preserves export progress as an NDJSON response", async () => {
  const body = '{"delivery":"current_state","job":{"sequence":1}}\n{"delivery":"current_state","job":{"sequence":2}}\n';
  const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(body, {
    headers: { "Content-Type": "application/x-ndjson" },
  }));
  const response = await api.getExportJobEvents("synthetic-job", { after: 1 }, { session: "synthetic-session" });
  expect(response).toBeInstanceOf(Response);
  expect(await response.text()).toBe(body);
  const [url, request] = fetch.mock.calls[0];
  expect(url).toBe("/api/v1/exports/jobs/synthetic-job/events?after=1");
  expect(new Headers(request?.headers).get("Accept")).toBe("application/x-ndjson");
  expect(new Headers(request?.headers).get("X-Docbank-Web-Session")).toBe("synthetic-session");
});

it.each(["end", "cancel"])("reads export progress incrementally before stream %s", async (finish) => {
  const running: api.GetExportJobEvents200 = {
    delivery: "current_state", requested_after: 0,
    job: {
      id: "synthetic-job", plan_id: "synthetic-plan", fingerprint: "a".repeat(64),
      attempt: 1, sequence: 1, state: "running", completed_bytes: 0, completed_roles: 0,
      created_at: "2026-09-01T00:00:00Z", deadline: "2026-09-01T02:00:00Z", expires_at: "2026-09-01T02:00:00Z",
    },
  };
  const failed = { ...running, job: { ...running.job, sequence: 2, state: "failed", failure: "archive_failed" } };
  const lastLine = JSON.stringify(failed) + "\n";
  const encoder = new TextEncoder();
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const cancel = vi.fn();
  const body = new ReadableStream<Uint8Array>({ start(value) { controller = value; }, cancel });
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(body, { headers: { "Content-Type": "application/x-ndjson" } }));
  const events = streamExportJobEvents("synthetic-job");
  try {
    controller.enqueue(encoder.encode(JSON.stringify(running) + "\n" + lastLine.slice(0, 20)));
    expect(await events.next()).toEqual({ done: false, value: running });
    if (finish === "cancel") {
      await events.return(undefined);
      expect(cancel).toHaveBeenCalledOnce();
    } else {
      controller.enqueue(encoder.encode(lastLine.slice(20)));
      controller.close();
      expect(await events.next()).toEqual({ done: false, value: failed });
      expect((await events.next()).done).toBe(true);
    }
    expect(body.locked).toBe(false);
  } finally {
    await events.return(undefined);
  }
});
