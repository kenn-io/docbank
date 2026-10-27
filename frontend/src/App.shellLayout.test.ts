import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import App from "./App.svelte";
import { root } from "./version-test-fixtures.js";
import { savePanelWidth } from "./panelWidth.js";
import { VerifiedUploadChannel } from "./upload.js";
import * as loadfile from "./loadfile.js";
import * as bates from "./bates.js";

beforeEach(() => localStorage.clear());
afterEach(() => {
  cleanup();
  sessionStorage.clear();
  history.replaceState(null, "", "/");
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it.each([
  ["shows Productions after importing load files without reloading", false],
  ["review discovers completed imports after closing the drawer", true],
] as const)("%s", async (_name, closeWhileQueued) => {
  vi.spyOn(VerifiedUploadChannel.prototype, "connect").mockResolvedValue();
  vi.spyOn(loadfile, "uploadPackageZIP").mockResolvedValue({
    container_id: "container",
    format: "zip",
    state: "sealed",
    sha256: "a".repeat(64),
    size: 3,
  });
  vi.spyOn(loadfile, "preflightPackageZIP").mockResolvedValue({
    preflight_id: "preview",
    source_kind: "container",
    source_ref: "container",
    profile_sha256: "a".repeat(64),
    mapping_sha256: "b".repeat(64),
    manifest_sha256: "c".repeat(64),
    volumes: [],
    records: 1,
    pages: 1,
    diagnostic_count: 0,
    diagnostics: [],
    blocking: false,
    created_at: "2026-09-21T00:00:00Z",
    expires_at: "2026-09-22T00:00:00Z",
  });
  const job = {
    operation_id: "operation",
    job_id: "job",
    package_id: "package",
    preflight_id: "preview",
    state: "queued",
    committed: 0,
    total: 1,
    gap_count: 0,
    gaps: [],
    created_at: "now",
    updated_at: "now",
  };
  vi.spyOn(loadfile, "startPackageImport").mockResolvedValue(job);
  const readImport = vi.spyOn(loadfile, "readPackageImport").mockResolvedValue({
    ...job,
    state: "complete",
    committed: 1,
  });
  start();
  await fireEvent.click(await screen.findByRole("button", { name: "Add" }));
  const importFiles = await screen.findByRole("menuitem", { name: "Import load files" });
  await waitFor(() => expect(importFiles.getAttribute("aria-disabled")).not.toBe("true"));
  await fireEvent.click(importFiles);
  await fireEvent.change(await screen.findByLabelText("Choose load-file ZIP"), {
    target: { files: [new File(["zip"], "review.zip")] },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Upload and preview" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Import package" }));
  expect(screen.queryByRole("button", { name: "Productions" })).toBeNull();
  if (closeWhileQueued) {
    await screen.findByText(/queued/);
    await fireEvent.click(screen.getByRole("button", { name: "Close load-file import" }));
    const original = vi.mocked(fetch).getMockImplementation()!;
    vi.mocked(fetch).mockImplementation(async (input, init) =>
      String(input).startsWith("/api/v1/packages?")
        ? Response.json({ items: [{ package_id: job.package_id, state: "complete" }] })
        : original(input, init),
    );
    await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
    expect(await screen.findByRole("button", { name: "Productions" })).toBeTruthy();
    expect(readImport).not.toHaveBeenCalled();
    return;
  }
  await waitFor(() => expect(screen.getByRole("button", { name: "Productions" })).toBeTruthy(), {
    timeout: 2000,
  });
});

it("shows Productions after creating a Bates export without reloading", async () => {
  const namespace = {
    namespace_id: "namespace",
    prefix: "CASE",
    suffix: "",
    padding: 6,
    created_at: "2026-09-21T00:00:00Z",
  };
  const labels = [
    { ordinal: 1, occurrence_id: "document", source_page: 1, output_page: 1, label: "CASE000001" },
  ];
  vi.spyOn(bates, "listBatesSources").mockResolvedValue([
    { package_id: "package", package_name: "Review set", snapshot_id: "snapshot", page_count: 1 },
  ]);
  vi.spyOn(bates, "listBatesNamespaces").mockResolvedValue([namespace]);
  vi.spyOn(bates, "previewBatesRange").mockResolvedValue({
    namespace,
    start_sequence: 1,
    end_sequence: 1,
    stamped_nothing: true,
    labels,
  });
  vi.spyOn(bates, "reserveBatesRange").mockResolvedValue({
    allocation_id: "allocation",
    namespace_id: "namespace",
    snapshot_id: "snapshot",
    recipe_sha256: "a".repeat(64),
    state: "reserved",
    start_sequence: 1,
    end_sequence: 1,
    labels,
    created_at: "2026-09-21T00:00:00Z",
  });
  vi.spyOn(bates, "startBatesExport").mockResolvedValue({
    artifact_id: "artifact",
    allocation_id: "allocation",
    blob_sha256: "b".repeat(64),
    size: 100,
    media_type: "application/pdf",
    page_count: 1,
    recipe_sha256: "a".repeat(64),
    manifest_sha256: "c".repeat(64),
    state: "verified",
    created_at: "2026-09-21T00:00:00Z",
    pages: [{ ...labels[0]!, source_blob_sha256: "d".repeat(64) }],
  });
  start();
  await fireEvent.click(await screen.findByRole("button", { name: "Export" }));
  await fireEvent.click(await screen.findByRole("menuitem", { name: "Bates-numbered PDFs…" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Preview Bates labels" }));
  await fireEvent.click(
    await screen.findByRole("button", { name: "Reserve CASE000001–CASE000001" }),
  );
  expect(screen.queryByRole("button", { name: "Productions" })).toBeNull();
  await fireEvent.click(await screen.findByRole("button", { name: "Start Bates export" }));
  expect(await screen.findByRole("button", { name: "Productions" })).toBeTruthy();
});

function start(sets: "empty" | "package" | "bates" = "empty", failedListing?: string) {
  history.replaceState(null, "", "/#web_session=synthetic&web_upload_secret=proof");
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === failedListing) return new Response("listing unavailable", { status: 500 });
    const value = url.startsWith("/api/v1/packages?")
      ? {
          items:
            sets === "package"
              ? [
                  {
                    package_id: "review",
                    package_name: "Review set",
                    direction: "received",
                    state: "complete",
                    page_count: 0,
                    member_count: 1,
                  },
                ]
              : [],
          limit: 250,
        }
      : url.startsWith("/api/v1/bates/exports?")
        ? {
            items: sets === "bates" ? [{ allocation_id: "export" }] : [],
            total: sets === "bates" ? 1 : 0,
          }
        : url.includes("/path?")
          ? root
          : url.includes("/children?")
            ? { directory: root, items: [], total: 0, limit: 1000, offset: 0 }
            : { items: [], total: 0, limit: 1000, offset: 0 };
    return new Response(JSON.stringify(value));
  });
  render(App);
}

it("tells the user the back button has nowhere to go instead of claiming a top folder", async () => {
  start();
  const back = await screen.findByRole("button", { name: "Back to previous directory" });
  await fireEvent.mouseEnter(back.parentElement!);
  expect((await screen.findByRole("tooltip")).textContent).toBe("No previous folder");
});

it("applies the saved inspector width", async () => {
  savePanelWidth("documents", 380);
  start();
  const workspace = await screen.findByRole("main");
  expect(workspace.style.getPropertyValue("--inspector-width")).toBe("min(380px, 50vw)");
});

it("shows a restore notice with Dismiss only and no Undo", async () => {
  start();
  const original = vi.mocked(fetch).getMockImplementation()!;
  const node = {
    ...root,
    id: 42,
    name: "Reports",
    path: "/Reports",
    trashed_at: "2026-07-28T12:00:00Z",
  };
  let restored = false;
  vi.mocked(fetch).mockImplementation(async (input, init) => {
    if (String(input).startsWith("/api/v1/trash?"))
      return Response.json({
        items: restored ? [] : [node],
        total: restored ? 0 : 1,
        limit: 1000,
        offset: 0,
      });
    if (String(input) === "/api/v1/nodes/42/restore") {
      restored = true;
      return Response.json({ ...node, trashed_at: undefined, revision: 2 });
    }
    return original(input, init);
  });
  await fireEvent.click((await screen.findAllByRole("button", { name: "Recoverable trash" }))[0]!);
  await fireEvent.click(await screen.findByRole("button", { name: "Restore" }));
  const dismiss = await screen.findByRole("button", { name: "Dismiss" });
  const notice = within(dismiss.closest('[role="status"]') as HTMLElement);
  expect(
    notice
      .getAllByRole("button")
      .map((button) => button.getAttribute("aria-label") ?? button.textContent?.trim()),
  ).toEqual(["Dismiss"]);
});

it("hides Productions when the vault has no sets", async () => {
  start();
  await screen.findByRole("button", { name: "Back to previous directory" });
  expect(screen.queryByRole("button", { name: "Productions" })).toBeNull();
});

it.each([
  { sets: "package", failedListing: "/api/v1/bates/exports?limit=1" },
  { sets: "bates", failedListing: "/api/v1/packages?limit=1" },
] as const)(
  "shows Productions when $sets loads and the other listing fails",
  async ({ sets, failedListing }) => {
    start(sets, failedListing);
    expect(await screen.findByRole("button", { name: "Productions" })).toBeTruthy();
  },
);

it("keeps Productions visible when refreshing a confirmed package listing fails", async () => {
  start("package");
  expect(await screen.findByRole("button", { name: "Productions" })).toBeTruthy();
  const original = vi.mocked(fetch).getMockImplementation()!;
  const failedListing = vi.fn(async () => new Response("listing unavailable", { status: 500 }));
  vi.mocked(fetch).mockImplementation(async (input, init) =>
    String(input).startsWith("/api/v1/packages?") ? failedListing() : original(input, init),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Refresh current view" }));
  await waitFor(() => expect(failedListing).toHaveBeenCalledOnce());
  expect(screen.getByRole("button", { name: "Productions" })).toBeTruthy();
});

it("opens production sets from the sidebar between Searches and Imports", async () => {
  start("package");
  const production = await screen.findByRole("button", { name: "Productions" });
  expect(production.previousElementSibling?.textContent).toContain("Searches");
  expect(production.nextElementSibling?.textContent).toContain("Imports");
  await fireEvent.click(production);
  expect(await screen.findByText("Review set")).toBeTruthy();
});
