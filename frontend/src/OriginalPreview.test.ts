import { createHash } from "node:crypto";
import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import OriginalPreview from "./OriginalPreview.svelte";
import { reloadWithSession } from "./browser-session.js";
import type { SelectedSource } from "./selectedSource.js";

vi.mock("./MarkdownPreview.svelte", () => {
  throw new Error("Reader chunk unavailable");
});
vi.mock("./browser-session.js", () => ({ reloadWithSession: vi.fn() }));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it("offers a choice before reloading after the reader fails to load", async () => {
  const bytes = new TextEncoder().encode("# Example");
  const hash = createHash("sha256").update(bytes).digest("hex");
  const source: SelectedSource = {
    kind: "snapshot",
    key: `snapshot:7:version:${hash}:${bytes.length}`,
    nodeID: 7,
    mutationRevision: 3,
    versionID: "11111111-1111-4111-8111-111111111111",
    blobHash: hash,
    size: bytes.length,
    name: "example.md",
    path: "/example.md",
    mimeType: "text/markdown",
    modifiedAt: "2026-09-10T12:00:00Z",
    observedAt: "2026-09-11T12:00:00Z",
    originalTags: [],
    collectionIDs: [],
  };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_input, init) => {
    if (init?.method === "POST")
      return new Response(
        `${JSON.stringify({
          phase: "ready",
          received: bytes.length,
          total: bytes.length,
          url: "/api/daemon/web-download/file?ticket=example",
          name: source.name,
          version_id: source.versionID,
          blob_hash: hash,
        })}\n`,
      );
    return new Response(bytes, {
      headers: {
        "Content-Type": source.mimeType,
        "Content-Length": String(bytes.length),
        "Content-Digest": `sha-256=:${createHash("sha256").update(bytes).digest("base64")}:`,
        "X-Docbank-Content-Version": source.versionID,
        "X-Docbank-Blob-Hash": hash,
        "X-Docbank-Blob-Size": String(bytes.length),
      },
    });
  });
  render(OriginalPreview, {
    session: "session",
    source,
    authorizationRevision: 8,
    onauthfailure: vi.fn(),
  });

  await fireEvent.click(await screen.findByRole("button", { name: "Reload Docbank" }));
  expect(screen.getByText("Reloading discards unsaved work in this tab.")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Keep working" }));
  expect(screen.getByRole("button", { name: "Reload Docbank" })).toBeTruthy();
  expect(reloadWithSession).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "Reload Docbank" }));
  await fireEvent.click(screen.getByRole("button", { name: "Reload anyway" }));
  expect(reloadWithSession).toHaveBeenCalledOnce();
});
