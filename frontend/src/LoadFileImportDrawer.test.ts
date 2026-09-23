import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import * as loadfile from "./loadfile.js";
import LoadFileImportDrawer from "./LoadFileImportDrawer.svelte";

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); });
const props = {
  session: "session",
  channel: { uploadPackageContainer: vi.fn() },
  destination: "/review",
  onclose: vi.fn(),
  oncomplete: vi.fn(),
  onauthfailure: vi.fn(),
};
const preview = {
  preflight_id: "preflight", source_kind: "container", source_ref: "source",
  profile_sha256: "a".repeat(64), mapping_sha256: "b".repeat(64), manifest_sha256: "c".repeat(64),
  volumes: [], records: 2, pages: 3, diagnostic_count: 0, diagnostics: [], blocking: false,
  created_at: "2026-09-21T00:00:00Z", expires_at: "2026-09-22T00:00:00Z",
};

it("requires a successful review before starting the durable import", async () => {
  vi.spyOn(loadfile, "uploadPackageZIP").mockResolvedValue({ container_id: "source", format: "zip", state: "sealed", sha256: "d".repeat(64), size: 3 });
  vi.spyOn(loadfile, "preflightPackageZIP").mockResolvedValue(preview);
  const start = vi.spyOn(loadfile, "startPackageImport").mockResolvedValue({
    operation_id: "operation", job_id: "job", package_id: "package", preflight_id: "preflight",
    state: "queued", committed: 0, total: 2, gap_count: 0, gaps: [], created_at: "now", updated_at: "now",
  });
  render(LoadFileImportDrawer, props);
  await fireEvent.change(screen.getByLabelText("Choose load-file ZIP"), { target: { files: [new File(["zip"], "review.zip")] } });
  expect(screen.queryByRole("button", { name: "Import package" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Upload and preview" }));
  expect(await screen.findByText("2")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Import package" }));
  expect(start).toHaveBeenCalledWith("session", expect.objectContaining({ into: "/review", preflight_id: "preflight", index_supplied_text: true }), expect.any(AbortSignal));
  expect(await screen.findByText("Package import")).toBeTruthy();
});

it("shows blocking diagnostics and disables import", async () => {
  vi.spyOn(loadfile, "uploadPackageZIP").mockResolvedValue({ container_id: "source", format: "zip", state: "sealed", sha256: "d".repeat(64), size: 3 });
  vi.spyOn(loadfile, "preflightPackageZIP").mockResolvedValue({ ...preview, blocking: true, diagnostic_count: 1, diagnostics: [{ code: "missing_native", severity: "blocking", detail: "Native is missing" }] });
  render(LoadFileImportDrawer, props);
  await fireEvent.change(screen.getByLabelText("Choose load-file ZIP"), { target: { files: [new File(["zip"], "review.zip")] } });
  await fireEvent.click(screen.getByRole("button", { name: "Upload and preview" }));
  expect(await screen.findByText("Native is missing")).toBeTruthy();
  expect((screen.getByRole("button", { name: "Import package" }) as HTMLButtonElement).disabled).toBe(true);
});
