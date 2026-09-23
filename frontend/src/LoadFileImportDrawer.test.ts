import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import * as loadfile from "./loadfile.js";
import LoadFileImportDrawer from "./LoadFileImportDrawer.svelte";

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); vi.unstubAllGlobals(); Reflect.deleteProperty(Element.prototype, "scrollIntoView"); });
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

it("retries preflight with corrected settings and uploads again when the ZIP changes", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  const upload = vi.spyOn(loadfile, "uploadPackageZIP").mockImplementation(async (_session, _channel, file, containerID) => ({
    container_id: containerID, format: "zip", state: "sealed", sha256: "d".repeat(64), size: file.size,
  }));
  const preflight = vi.spyOn(loadfile, "preflightPackageZIP")
    .mockRejectedValueOnce(new Error("Invalid source encoding"))
    .mockResolvedValue(preview);
  render(LoadFileImportDrawer, props);
  await fireEvent.change(screen.getByLabelText("Choose load-file ZIP"), { target: { files: [new File(["zip"], "review.zip")] } });
  await fireEvent.click(screen.getByRole("button", { name: "Upload and preview" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Invalid source encoding");
  const firstID = upload.mock.calls[0][3];
  expect(preflight).toHaveBeenNthCalledWith(1, "session", firstID, "dat-concordance-v1", "", "utf-8", expect.any(AbortSignal));

  await fireEvent.click(screen.getByRole("combobox", { name: "Source encoding: UTF-8" }));
  await fireEvent.click(screen.getByRole("option", { name: "Windows-1252" }));
  await fireEvent.click(screen.getByRole("button", { name: "Upload and preview" }));
  expect(await screen.findByText("Package preview")).toBeTruthy();
  expect(screen.queryByRole("alert")).toBeNull();
  expect(upload).toHaveBeenCalledTimes(1);
  expect(preflight).toHaveBeenNthCalledWith(2, "session", firstID, "dat-concordance-v1", "", "windows-1252", expect.any(AbortSignal));

  const replacement = new File(["new zip"], "replacement.zip");
  await fireEvent.change(screen.getByLabelText("Choose load-file ZIP"), { target: { files: [replacement] } });
  expect(screen.queryByText("Package preview")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Upload and preview" }));
  expect(await screen.findByText("Package preview")).toBeTruthy();
  expect(upload).toHaveBeenCalledTimes(2);
  const nextID = upload.mock.calls[1][3];
  expect(nextID).not.toBe(firstID);
  expect(upload).toHaveBeenNthCalledWith(2, "session", props.channel, replacement, nextID, expect.any(AbortSignal), expect.any(Function));
  expect(preflight).toHaveBeenNthCalledWith(3, "session", nextID, "dat-concordance-v1", "", "windows-1252", expect.any(AbortSignal));
});
