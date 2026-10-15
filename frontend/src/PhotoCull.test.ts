import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { PhotoAuthoredEditor } from "./photoAuthored.svelte.js";
import PhotoCullView from "./PhotoCull.svelte";
import { PhotoCull } from "./photoCull.svelte.js";
import { Photos } from "./photos.svelte.js";
import { photo } from "./photo-test-fixtures.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";
import type { CullRun } from "./photoCullStorage.js";
import type { PhotoAuthoredReceipt } from "./generated/docbank.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
it("decides once with P, advances, announces the decision and offers receipt Undo", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const rows = [photo(1), photo(2)];
  const run: CullRun = {
    ids: rows.map((row) => row.asset_id),
    checkpoint: {
      runID: "run",
      pass: rows.map((row) => row.asset_id),
      position: 0,
      visited: [rows[0]!.asset_id],
      skipped: [],
      unavailable: [],
      scope: "unrated",
      autoAdvance: true,
    },
  };
  const cull = new PhotoCull("session", {
    load: async () => run,
    replace: vi.fn(),
    checkpoint: vi.fn(),
  });
  await cull.restore();
  const photos = new Photos("session", vi.fn());
  vi.spyOn(photos, "lookup").mockImplementation(
    async (id) => rows.find((row) => row.asset_id === id)!,
  );
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  const receipt = {
    after: [{ file_id: rows[0]!.display_file_id, values: { flag: "pick", rating: 0 } }],
  } as unknown as PhotoAuthoredReceipt;
  const edit = vi
    .spyOn(PhotoAuthoredEditor.prototype, "edit")
    .mockImplementation(
      async function (this: PhotoAuthoredEditor, _scope, _patch, _hidden, refresh) {
        this.receiptID = "decision-receipt";
        this.undoState.summary = "Photo picked";
        await refresh(receipt, []);
      },
    );
  vi.spyOn(photos, "metadataChanged").mockReturnValue(new Map());
  const undo = vi.spyOn(PhotoAuthoredEditor.prototype, "undo").mockResolvedValue();
  const close = vi.fn();
  render(PhotoCullView, {
    cull,
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    onclose: close,
  });
  await screen.findByText("Photo 1.jpg");
  const dialog = screen.getByRole("dialog", { name: "Cull photos" });
  await fireEvent.keyDown(dialog, { key: "p", ctrlKey: true });
  expect(edit).not.toHaveBeenCalled();
  await fireEvent.keyDown(dialog, { key: "p" });
  await waitFor(() => expect(cull.currentID).toBe(rows[1]!.asset_id));
  expect(edit.mock.calls[0]?.slice(0, 3)).toEqual([
    { asset_ids: [rows[0]!.asset_id] },
    { flag: "pick" },
    false,
  ]);
  expect(cull.run?.checkpoint.pass).toEqual([rows[0]!.asset_id, rows[1]!.asset_id]);
  expect(document.querySelector(".kit-sr-only")?.textContent).toContain("Photo 1.jpg: pick");
  await fireEvent.click(screen.getByRole("button", { name: /^Undo$/ }));
  expect(undo).toHaveBeenCalled();
  await fireEvent.keyDown(dialog, { key: "Escape" });
  expect(close).toHaveBeenCalledOnce();
});
it("keeps a failed decision on screen and guards exit while its draft remains", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const row = photo(1);
  const cull = new PhotoCull("session", {
    load: async () => undefined,
    replace: vi.fn(),
    checkpoint: vi.fn(),
  });
  cull.run = {
    ids: [row.asset_id],
    checkpoint: {
      runID: "run",
      pass: [row.asset_id],
      position: 0,
      visited: [],
      skipped: [],
      unavailable: [],
      scope: "all",
      autoAdvance: true,
    },
  };
  const photos = new Photos("session", vi.fn());
  vi.spyOn(photos, "lookup").mockResolvedValue(row);
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  vi.spyOn(PhotoAuthoredEditor.prototype, "edit").mockImplementation(
    async function (this: PhotoAuthoredEditor) {
      this.stage({ flag: "reject" });
      this.error = "Photo changed";
    },
  );
  const close = vi.fn();
  render(PhotoCullView, {
    cull,
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    onclose: close,
  });
  await screen.findByText("Photo 1.jpg");
  await fireEvent.keyDown(screen.getByRole("dialog"), { key: "x" });
  await screen.findByText("Photo changed");
  expect(cull.currentID).toBe(row.asset_id);
  await fireEvent.click(screen.getByRole("button", { name: "Exit cull" }));
  expect(close).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "Clear failed edit" }));
  await fireEvent.click(screen.getByRole("button", { name: "Exit cull" }));
  expect(close).toHaveBeenCalledOnce();
});
