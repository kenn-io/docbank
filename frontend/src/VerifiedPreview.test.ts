import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import VerifiedPreview from "./VerifiedPreview.svelte";
import type { SelectedSource } from "./selectedSource.js";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function source(
  content: Uint8Array,
  versionID: string,
  mimeType = "text/plain",
): SelectedSource {
  const blobHash = createHash("sha256").update(content).digest("hex");
  return {
    kind: "snapshot",
    key: `snapshot:7:${versionID}:${blobHash}:${content.length}`,
    nodeID: 7,
    mutationRevision: 3,
    versionID,
    blobHash,
    size: content.length,
    name: mimeType.startsWith("image/") ? "scan.png" : "report.txt",
    path: mimeType.startsWith("image/") ? "/scan.png" : "/report.txt",
    mimeType,
    modifiedAt: "2026-09-10T12:00:00Z",
    observedAt: "2026-09-11T12:00:00Z",
    originalTags: [],
    collectionIDs: [],
  };
}

function ready(selected: SelectedSource, ticket: string): Response {
  return new Response(
    `${JSON.stringify({
      phase: "ready",
      received: selected.size,
      total: selected.size,
      url: `/api/daemon/web-download/file?ticket=${ticket}`,
      name: selected.name,
      version_id: selected.versionID,
      blob_hash: selected.blobHash,
    })}\n`,
  );
}

function body(selected: SelectedSource, bytes: Uint8Array): Response {
  return new Response(Uint8Array.from(bytes).buffer, {
    headers: {
      "Content-Type": selected.mimeType,
      "Content-Length": String(bytes.length),
      "Content-Digest": `sha-256=:${createHash("sha256").update(bytes).digest("base64")}:`,
      "X-Docbank-Content-Version": selected.versionID,
      "X-Docbank-Blob-Hash": selected.blobHash,
      "X-Docbank-Blob-Size": String(selected.size),
    },
  });
}

it("shows only the newly selected exact version when source and session change", async () => {
  const oldBytes = new TextEncoder().encode("old frozen text");
  const newBytes = new TextEncoder().encode("new selected text");
  const oldSource = source(oldBytes, "11111111-1111-4111-8111-111111111111");
  const newSource = source(newBytes, "22222222-2222-4222-8222-222222222222");
  let oldBodySignal: AbortSignal | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const path = String(input);
    if (init?.method === "POST") {
      const request = JSON.parse(String(init.body)) as { version_id: string };
      return request.version_id === oldSource.versionID
        ? ready(oldSource, "old")
        : ready(newSource, "new");
    }
    if (path.endsWith("ticket=old") && init?.method !== "DELETE") {
      oldBodySignal = init?.signal ?? undefined;
      return new Promise<Response>((_resolve, reject) => {
        init?.signal?.addEventListener("abort", () =>
          reject(new DOMException("cancelled", "AbortError")),
        );
      });
    }
    if (path.endsWith("ticket=new")) return body(newSource, newBytes);
    if (init?.method === "DELETE") return new Response(null, { status: 204 });
    throw new Error(`unexpected request ${path}`);
  });
  const view = render(VerifiedPreview, {
    session: "first-session",
    source: oldSource,
    authorizationRevision: 8,
    onauthfailure: vi.fn(),
  });
  await waitFor(() => expect(oldBodySignal).toBeTruthy());

  await view.rerender({
    session: "second-session",
    source: newSource,
    authorizationRevision: 9,
    onauthfailure: vi.fn(),
  });

  expect(await screen.findByText("new selected text")).toBeTruthy();
  expect(oldBodySignal?.aborted).toBe(true);
  expect(screen.queryByText("old frozen text")).toBeNull();
});

it("publishes a verified raster URL and revokes it on teardown", async () => {
  const bytes = new Uint8Array([137, 80, 78, 71]);
  const selected = source(
    bytes,
    "33333333-3333-4333-8333-333333333333",
    "image/jpeg",
  );
  vi.spyOn(globalThis, "fetch")
    .mockResolvedValueOnce(ready(selected, "image"))
    .mockResolvedValueOnce(body(selected, bytes));
  const createObjectURL = vi.fn(() => "blob:verified-image");
  const revokeObjectURL = vi.fn();
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: createObjectURL,
  });
  Object.defineProperty(URL, "revokeObjectURL", {
    configurable: true,
    value: revokeObjectURL,
  });

  const view = render(VerifiedPreview, {
    session: "session",
    source: selected,
    authorizationRevision: 8,
    onauthfailure: vi.fn(),
  });
  expect(
    (
      await screen.findByRole("img", { name: "Verified preview of scan.png" })
    ).getAttribute("src"),
  ).toBe("blob:verified-image");
  expect(createObjectURL).toHaveBeenCalledOnce();

  view.unmount();
  expect(revokeObjectURL).toHaveBeenCalledWith("blob:verified-image");
});

it("keeps unsupported content on the verified-download fallback without a preview request", async () => {
  const selected = source(
    new TextEncoder().encode("<svg/>"),
    "44444444-4444-4444-8444-444444444444",
    "image/svg+xml",
  );
  const fetchMock = vi.spyOn(globalThis, "fetch");

  render(VerifiedPreview, {
    session: "session",
    source: selected,
    authorizationRevision: 8,
    onauthfailure: vi.fn(),
  });

  expect(
    await screen.findByText(/not an eligible text or raster image preview/i),
  ).toBeTruthy();
  expect(
    screen.getByRole("button", { name: "Download verified original" }),
  ).toBeTruthy();
  expect(fetchMock).not.toHaveBeenCalled();
});

it("loads one exact rendition only while Text is active and keeps hostile markup inert", async () => {
  const original = new TextEncoder().encode("%PDF synthetic");
  const selected = source(original, "55555555-5555-4555-8555-555555555555", "application/pdf");
  const text = new TextEncoder().encode("Alpha 😀 <script>alert('blocked')</script> alpha");
  const digest = createHash("sha256").update(text).digest("hex");
  const authority = {
    profile: "b".repeat(64), generation: "c".repeat(64), attachment: "d".repeat(64),
    build: "e".repeat(64), artifact: `artifact_${"f".repeat(64)}`,
  };
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const path = String(input);
    if (path.endsWith("/api/v1/pages/inventory")) return new Response(JSON.stringify({ runtime_available: false,
      inventory: { source: { version_id: selected.versionID, sha256: selected.blobHash, size: selected.size }, page_count: 0, frames: [], recipes: [], images: [] } }));
    if (path.endsWith("/api/v1/renditions/text")) return new Response(JSON.stringify({
      state: "ready",
      source: { node_id: selected.nodeID, revision: 8, version_id: selected.versionID,
        blob_hash: selected.blobHash, size: selected.size, media_type: selected.mimeType },
      profile: { name: "archive", configuration: "configured", fingerprint: authority.profile },
      generation_id: authority.generation, attachment_id: authority.attachment, build_id: authority.build,
      artifact: { id: authority.artifact, sha256: digest, size: text.length,
        media_type: "text/markdown; charset=utf-8" },
    }), { headers: { "Content-Type": "application/json" } });
    if (path.includes("/api/v1/renditions/text/content?")) return new Response(text, { headers: {
      "Content-Type": "text/markdown; charset=utf-8", "Content-Length": String(text.length),
      "Content-Digest": `sha-256=:${createHash("sha256").update(text).digest("base64")}:`,
      "X-Docbank-Rendition-SHA256": digest, "X-Docbank-Rendition-Size": String(text.length),
      "X-Docbank-Rendition-Profile": authority.profile,
      "X-Docbank-Rendition-Generation": authority.generation,
      "X-Docbank-Rendition-Attachment": authority.attachment,
      "X-Docbank-Rendition-Build": authority.build,
      "X-Docbank-Rendition-Artifact": authority.artifact,
    } });
    throw new Error(`unexpected request ${path}`);
  });
  const props = {
    session: "session", source: selected, authorizationRevision: 8, profileName: "archive",
    observed: { configuration: "configured" as const, profileFingerprint: authority.profile,
      generationID: authority.generation, coverageState: "complete" as const,
      attachmentID: authority.attachment, buildID: authority.build },
    queryTerms: ["alpha"], onauthfailure: vi.fn(),
  };
  const view = render(VerifiedPreview, props);
  await screen.findByText(/No page inventory/);
  expect(fetchMock).toHaveBeenCalledTimes(1);
  await fireEvent.click(screen.getByRole("tab", { name: "Text" }));
  expect(await screen.findByText(/<script>alert\('blocked'\)<\/script>/)).toBeTruthy();
  expect(document.querySelector("script")).toBeNull();
  expect(document.querySelectorAll("mark[data-source='query']")).toHaveLength(2);
  expect(fetchMock).toHaveBeenCalledTimes(3);

  await view.rerender({ ...props, queryTerms: ["alpha", "blocked"] });
  await waitFor(() => expect(document.querySelectorAll("mark")).toHaveLength(3));
  expect(fetchMock).toHaveBeenCalledTimes(3);
});
