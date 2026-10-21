import * as actions from "./action-events.js";
import * as downloads from "./download.js";
import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import DownloadButton from "./DownloadButton.svelte";
import type { SelectedSource } from "./selectedSource.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function source(version: string, hash: string): SelectedSource {
  return {
    kind: "live",
    key: `live:7:${version}:${hash}:12`,
    nodeID: 7,
    mutationRevision: 3,
    versionID: version,
    blobHash: hash,
    size: 12,
    name: "report.txt",
    path: "/report.txt",
    mimeType: "text/plain",
    modifiedAt: "2026-09-11T12:00:00Z",
  };
}

it("aborts an in-flight exact download on same-node version or session change", async () => {
  const first = source("11111111-1111-4111-8111-111111111111", "a".repeat(64));
  const second = source("22222222-2222-4222-8222-222222222222", "b".repeat(64));
  const signals: AbortSignal[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_path, init) => {
    if (init?.method === "DELETE") return new Response(null, { status: 204 });
    if (init?.signal) signals.push(init.signal);
    return new Promise<Response>(() => undefined);
  });
  const view = render(DownloadButton, {
    session: "first-session",
    source: first,
    authorizationRevision: 9,
    onauthfailure: vi.fn(),
  });

  await fireEvent.click(screen.getByRole("button", { name: "Download" }));
  await waitFor(() => expect(signals).toHaveLength(1));
  await view.rerender({
    session: "second-session",
    source: second,
    authorizationRevision: 10,
    onauthfailure: vi.fn(),
  });

  expect(signals[0]?.aborted).toBe(true);
  expect(screen.getByRole("button", { name: "Download" })).toBeTruthy();
  expect(screen.queryByText(/Verified/)).toBeNull();
});

it.each([true, false])("reports only a verified completed download (%s)", async (succeeds) => {
  const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
  const selected = source("11111111-1111-4111-8111-111111111111", "a".repeat(64));
  const prepare = vi.spyOn(downloads, "prepareExactDownload");
  if (succeeds)
    prepare.mockResolvedValue({
      url: "/verified",
      name: "report.txt",
      versionID: selected.versionID,
      blobHash: selected.blobHash,
      size: 12,
    });
  else prepare.mockRejectedValue(new Error("Digest mismatch"));
  const offer = vi.spyOn(downloads, "offerPreparedDownload").mockImplementation(() => {});
  render(DownloadButton, {
    session: "session",
    source: selected,
    authorizationRevision: 9,
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("button", { name: "Download" }));
  await screen.findByText(succeeds ? /browser save started/ : "Digest mismatch");
  expect(action.mock.calls).toEqual(succeeds ? [["session", "document_opened"]] : []);
  expect(offer).toHaveBeenCalledTimes(succeeds ? 1 : 0);
});
