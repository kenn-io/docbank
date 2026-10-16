import { afterEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { PhotoAuthoredEditor } from "./photoAuthored.svelte.js";
import PhotoCullView from "./PhotoCull.svelte";
import { PhotoCull } from "./photoCull.svelte.js";
import { Photos } from "./photos.svelte.js";
import { photo, preparedPhoto } from "./photo-test-fixtures.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";
import type { CullRun } from "./photoCullStorage.js";
import * as api from "./generated/docbank.js";
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
  const rows = [photo(1), { ...photo(2), rating: 5 }];
  const run: CullRun = {
    ids: rows.map((row) => row.asset_id),
    checkpoint: {
      runID: "run",
      passID: "pass",
      revision: 0,
      pass: rows.map((row) => row.asset_id),
      position: 0,
      visited: [rows[0]!.asset_id],
      skipped: [],
      unavailable: [],
      scope: "unflagged",
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
  vi.spyOn(photos, "lookupMany").mockResolvedValue([]);
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
  await fireEvent.keyDown(dialog, { key: "p" });
  await waitFor(() => expect(cull.currentID).toBe(rows[1]!.asset_id));
  expect(edit.mock.calls[0]?.slice(0, 3)).toEqual([
    { asset_ids: [rows[0]!.asset_id] },
    { flag: "pick" },
    false,
  ]);
  expect(document.querySelector(".kit-sr-only")?.textContent).toContain("Photo 1.jpg: pick");
  await fireEvent.click(screen.getByRole("button", { name: /^Undo$/ }));
  expect(undo).toHaveBeenCalled();
  await screen.findByText("Photo 2.jpg");
  await fireEvent.keyDown(dialog, { key: "0" });
  await waitFor(() => expect(edit).toHaveBeenCalledTimes(2));
  expect(edit.mock.calls[1]?.slice(0, 3)).toEqual([
    { asset_ids: [rows[1]!.asset_id] },
    { rating: 0 },
    false,
  ]);
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
      passID: "pass",
      revision: 0,
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
  vi.spyOn(photos, "lookupMany").mockResolvedValue([]);
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

it.each([false, true])(
  "refreshes the visible completion offer after Undo (older lookup pending: %s)",
  async (pending) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    const row = photo(1),
      before = preparedPhoto(1);
    const picked: api.PhotoAuthoredSnapshot = {
      file_id: before.file_id,
      node_id: 1,
      revision: 2,
      values: { ...before.values, flag: "pick" },
    };
    const restored = { ...picked, revision: 3, values: { ...before.values } };
    vi.spyOn(api, "preparePhotoAuthored").mockResolvedValue({ assets: [], targets: [before] });
    vi.spyOn(api, "editPhotoAuthored").mockImplementation(async () => {
      row.flag = "pick";
      return { receipt_id: "pick", before: [before], after: [picked] };
    });
    vi.spyOn(api, "undoPhotoAuthored").mockImplementation(async () => {
      row.flag = "";
      return { receipt_id: "undo", before: [picked], after: [restored] };
    });
    const stalePage: api.PhotoBrowsePage = {
      items: [{ ...row, flag: "pick" }],
      total: 1,
      facets: [],
      relevance_available: false,
    };
    let release!: (page: typeof stalePage) => void;
    const oldLookup = new Promise<typeof stalePage>((resolve) => {
      release = resolve;
    });
    const list = vi
      .spyOn(api, "listPhotoAssets")
      .mockImplementation(async () => ({ ...stalePage, items: [{ ...row }] }));
    if (pending) list.mockImplementationOnce(() => oldLookup);
    const cull = new PhotoCull("session", {
      load: async () => undefined,
      replace: vi.fn(),
      checkpoint: vi.fn(),
    });
    cull.run = {
      ids: [row.asset_id],
      checkpoint: {
        runID: "run",
        passID: "pass",
        revision: 0,
        pass: [row.asset_id],
        position: 0,
        visited: [row.asset_id],
        skipped: [],
        unavailable: [],
        scope: "unflagged",
        autoAdvance: true,
      },
    };
    const photos = new Photos("session", vi.fn());
    photos.items = [row];
    vi.spyOn(photos, "lookup").mockResolvedValue(row);
    vi.spyOn(photos, "lookupMany").mockResolvedValue([]);
    vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
    render(PhotoCullView, {
      cull,
      photos,
      cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
      onclose: vi.fn(),
    });
    await screen.findByText("Photo 1.jpg");
    await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
    await screen.findByRole("heading", { name: "Pass complete" });
    await waitFor(() => expect(list).toHaveBeenCalledTimes(1));
    if (!pending) await waitFor(() => expect(cull.busy).toBe(false));
    expect(screen.queryByRole("button", { name: "Review unflagged remainder" })).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: /^Undo$/ }));
    await screen.findByRole("button", { name: "Review unflagged remainder" });
    if (pending) {
      expect(list.mock.calls[0]![1]!.signal!.aborted).toBe(true);
      await act(async () => {
        release(stalePage);
        await oldLookup;
      });
      expect(screen.getByRole("button", { name: "Review unflagged remainder" })).toBeTruthy();
    }
  },
);

it("keeps the current photo and its pending decision when auto-advance is toggled", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const rows = [photo(1), photo(2)];
  const cull = new PhotoCull("session", {
    load: async () => undefined,
    replace: vi.fn(),
    checkpoint: vi.fn(),
  });
  cull.run = {
    ids: rows.map((row) => row.asset_id),
    checkpoint: {
      runID: "run",
      passID: "pass",
      revision: 0,
      pass: rows.map((row) => row.asset_id),
      position: 0,
      visited: [],
      skipped: [],
      unavailable: [],
      scope: "all",
      autoAdvance: true,
    },
  };
  const photos = new Photos("session", vi.fn());
  const lookup = vi
    .spyOn(photos, "lookup")
    .mockImplementation(async (id) => rows.find((row) => row.asset_id === id)!);
  vi.spyOn(photos, "lookupMany").mockResolvedValue([]);
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  let save!: () => void;
  vi.spyOn(PhotoAuthoredEditor.prototype, "edit").mockImplementation(
    () => new Promise<void>((resolve) => (save = resolve)),
  );
  render(PhotoCullView, {
    cull,
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    onclose: vi.fn(),
  });
  await screen.findByText("Photo 1.jpg");
  const dialog = screen.getByRole("dialog", { name: "Cull photos" });
  await fireEvent.keyDown(dialog, { key: "p" });
  await waitFor(() => expect(save).toBeDefined());
  lookup.mockImplementation(() => new Promise(() => {}));
  await fireEvent.click(screen.getByRole("button", { name: "Auto-advance: on" }));
  await screen.findByRole("button", { name: "Auto-advance: off" });
  const stillShown = !!screen.queryByText("Photo 1.jpg");
  save();
  await new Promise((resolve) => setTimeout(resolve, 20));
  // The same photo stays on screen and is not looked up again.
  expect(stillShown).toBe(true);
  expect(lookup).toHaveBeenCalledTimes(1);
  expect(document.querySelector(".kit-sr-only")?.textContent).toContain("Photo 1.jpg: pick");
  expect(cull.currentID).toBe(rows[0]!.asset_id);
});
