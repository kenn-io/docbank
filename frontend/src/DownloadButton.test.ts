import * as actions from "./action-events.js";
import * as downloads from "./download.js";
import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import DownloadButton from "./DownloadButton.svelte";
import { APIError } from "./api-transport.js";
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

it.each([
  {
    name: "success",
    succeeds: true,
    cancels: false,
    message: /browser save started/,
    cause: undefined,
  },
  {
    name: "failure",
    succeeds: false,
    cancels: false,
    message: "Digest mismatch",
    cause: new Error("Digest mismatch"),
  },
  {
    name: "cancel",
    succeeds: false,
    cancels: true,
    message: /cancelled/,
    cause: new DOMException("The operation was aborted.", "AbortError"),
  },
])(
  "reports only a verified completed download ($name)",
  async ({ succeeds, cancels, message, cause }) => {
    const action = vi.spyOn(actions, "reportAction").mockImplementation(() => {});
    const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
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
    else if (cancels)
      prepare.mockImplementation(
        (_session, _source, _revision, _mode, signal) =>
          new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(cause))),
      );
    else prepare.mockRejectedValue(cause);
    const offer = vi.spyOn(downloads, "offerPreparedDownload").mockImplementation(() => {});
    render(DownloadButton, {
      session: "session",
      source: selected,
      authorizationRevision: 9,
      onauthfailure: vi.fn(),
    });
    await fireEvent.click(screen.getByRole("button", { name: "Download" }));
    if (cancels) await fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await screen.findByText(message);
    expect(action.mock.calls).toEqual(succeeds ? [["session", "document_opened"]] : []);
    expect(failure.mock.calls).toEqual(succeeds || cancels ? [] : [["session", "open", cause]]);
    expect(offer).toHaveBeenCalledTimes(succeeds ? 1 : 0);
  },
);

it("leaves sign-in failures to the authentication handler", async () => {
  const failure = vi.spyOn(actions, "reportFailure").mockImplementation(() => {});
  const cause = new APIError("Unauthorized", 401, "unauthorized");
  vi.spyOn(downloads, "prepareExactDownload").mockRejectedValue(cause);
  const onauthfailure = vi.fn();
  render(DownloadButton, {
    session: "session",
    source: source("11111111-1111-4111-8111-111111111111", "a".repeat(64)),
    onauthfailure,
  });

  await fireEvent.click(screen.getByRole("button", { name: "Download" }));
  await waitFor(() => expect(onauthfailure).toHaveBeenCalledWith(cause));
  expect(failure).not.toHaveBeenCalled();
});
