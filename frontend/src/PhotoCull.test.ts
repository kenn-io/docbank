import { afterEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { PhotoAuthoredEditor } from "./photoAuthored.svelte.js";
import { APIError } from "./api-transport.js";
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
  delete (HTMLImageElement.prototype as { decode?: unknown }).decode;
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
    saved: vi.fn(),
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
  await fireEvent.keyDown(document.querySelector(".photo-cull")!, { key: "p" });
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
  await fireEvent.keyDown(document.querySelector(".photo-cull")!, { key: "0" });
  await waitFor(() => expect(edit).toHaveBeenCalledTimes(2));
  expect(edit.mock.calls[1]?.slice(0, 3)).toEqual([
    { asset_ids: [rows[1]!.asset_id] },
    { rating: 0 },
    false,
  ]);
  await fireEvent.keyDown(document.querySelector(".photo-cull")!, { key: "Escape" });
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
    saved: vi.fn(),
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
  await fireEvent.keyDown(document.querySelector(".photo-cull")!, { key: "x" });
  await screen.findByText("Photo changed");
  expect(cull.currentID).toBe(row.asset_id);
  await fireEvent.click(screen.getByRole("button", { name: "Exit cull" }));
  expect(close).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "Discard edit" }));
  await fireEvent.click(screen.getByRole("button", { name: "Exit cull" }));
  expect(close).toHaveBeenCalledOnce();
});

it("refreshes the visible completion offer after Undo", async () => {
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
  const list = vi.spyOn(api, "listPhotoAssets").mockImplementation(async (request) => ({
    ...stalePage,
    items:
      !request.query.filters?.flags || request.query.filters.flags.includes(row.flag)
        ? [{ ...row }]
        : [],
  }));
  const cull = new PhotoCull("session", {
    saved: vi.fn(),
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
  await waitFor(() => expect(list).toHaveBeenCalled());
  await waitFor(() => expect(cull.busy).toBe(false));
  expect(screen.queryByRole("button", { name: "Review unflagged remainder" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: /^Undo$/ }));
  await screen.findByRole("button", { name: "Review unflagged remainder" });
});

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
    saved: vi.fn(),
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
  vi.spyOn(photos, "lookup").mockImplementation(
    async (id) => rows.find((row) => row.asset_id === id)!,
  );
  vi.spyOn(photos, "lookupMany").mockResolvedValue([]);
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  let save!: () => void;
  const saving = new Promise<void>((resolve) => (save = resolve));
  vi.spyOn(PhotoAuthoredEditor.prototype, "edit").mockReturnValue(saving);
  render(PhotoCullView, {
    cull,
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    onclose: vi.fn(),
  });
  await screen.findByText("Photo 1.jpg");
  await fireEvent.keyDown(document.querySelector(".photo-cull")!, { key: "p" });
  await fireEvent.click(screen.getByRole("button", { name: "Auto-advance: on" }));
  await screen.findByRole("button", { name: "Auto-advance: off" });
  await act(async () => {
    save();
    await saving;
    await cull.flush();
  });
  expect(document.querySelector(".kit-sr-only")?.textContent).toContain("Photo 1.jpg: pick");
  expect(cull.currentID).toBe(rows[0]!.asset_id);
});

async function reviewPhoto(row = photo(1), autoAdvance = false) {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const cull = new PhotoCull("session", {
    saved: vi.fn(),
    load: vi.fn(),
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
      autoAdvance,
    },
  };
  const photos = new Photos("session", vi.fn());
  vi.spyOn(photos, "lookup").mockResolvedValue(row);
  vi.spyOn(photos, "lookupMany").mockResolvedValue([row]);
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  const view = render(PhotoCullView, {
    cull,
    photos,
    cache: {
      get: vi.fn(async () => new Blob(["synthetic-photo"])),
    } as unknown as PhotoPreviewCache,
    onclose: vi.fn(),
  });
  await screen.findByText(row.name);
  return { cull, photos, ...view };
}

it("shows the restored rotation in the cull stage after Undo", async () => {
  let url = 0;
  vi.spyOn(URL, "createObjectURL").mockImplementation(() => `blob:rotation-${++url}`);
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: async () => {},
  });
  const row = { ...photo(1), rotation: 90 as const };
  row.previews.grid = { state: "ready", generation_id: "grid-1" };
  const before = preparedPhoto(1);
  vi.spyOn(api, "undoPhotoAuthored").mockResolvedValue({
    receipt_id: "undo-rotation",
    before: [],
    after: [
      {
        file_id: before.file_id,
        node_id: before.node_id,
        revision: 3,
        values: { ...before.values, rotation: 0 },
      },
    ],
  });
  const { photos, container } = await reviewPhoto(row);
  await act(() => {
    photos.cullEditor!.receiptID = "rotation";
    photos.cullEditor!.undoState.summary = "Rotated photo";
  });
  // The stage passes its authored angle to the image's rotation layer.
  await waitFor(() =>
    expect(container.querySelector(".photo-stage [style*='rotate(90deg)']")).toBeTruthy(),
  );
  const preview = container.querySelector(".photo-stage img")!.getAttribute("src");
  await fireEvent.click(screen.getByRole("button", { name: /^Undo$/ }));
  await waitFor(() =>
    expect(container.querySelector(".photo-stage [style*='rotate(0deg)']")).toBeTruthy(),
  );
  expect(container.querySelector(".photo-stage img")!.getAttribute("src")).toBe(preview);
});

it.each([false, true])(
  "reloads a 412 and retries the kept decision after member review=%s",
  async (membershipChanged) => {
    const before = preparedPhoto(1),
      extra = preparedPhoto(1, { file_id: "other-file", file_name: "Other.jpg" });
    const prepare = vi
      .spyOn(api, "preparePhotoAuthored")
      .mockResolvedValueOnce({
        assets: [],
        targets: membershipChanged ? [before, extra] : [before],
      })
      .mockResolvedValue({ assets: [], targets: [{ ...before, revision: 2 }] });
    const edit = vi
      .spyOn(api, "editPhotoAuthored")
      .mockRejectedValueOnce(new APIError("Photo changed", 412, "precondition_failed"))
      .mockResolvedValue({
        receipt_id: "pick",
        before: [before],
        after: [
          {
            file_id: before.file_id,
            node_id: before.node_id,
            revision: 3,
            values: { ...before.values, flag: "pick" },
          },
        ],
      });
    vi.spyOn(api, "listPhotoAssets").mockResolvedValue({
      items: [],
      total: 0,
      facets: [],
      relevance_available: false,
    });
    const { cull, container } = await reviewPhoto(photo(1), true);
    await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
    await screen.findByText("Photo changed");
    expect(document.activeElement).toBe(container.querySelector(".photo-cull"));
    expect(cull.currentID).toBe("photo-1");
    await fireEvent.click(screen.getByRole("button", { name: "Reload and retry" }));
    if (membershipChanged) {
      await screen.findByRole("list", { name: "Members to review" });
      expect(edit).toHaveBeenCalledTimes(1);
      expect(screen.getByRole("list", { name: "Members to review" }).textContent).toContain(
        "Photo 1.jpg",
      );
      await fireEvent.click(screen.getByRole("button", { name: "Use reviewed members" }));
      await fireEvent.click(screen.getByRole("button", { name: "Reload and retry" }));
    }
    await screen.findByRole("heading", { name: "Pass complete" });
    expect(prepare).toHaveBeenCalledTimes(2);
    expect(edit).toHaveBeenCalledTimes(2);
    expect(edit.mock.calls[1]![0]).toMatchObject({
      patch: { flag: "pick" },
      targets: [{ file_id: before.file_id, revision: 2 }],
    });
    expect(document.activeElement).toBe(container.querySelector(".photo-cull"));
    await waitFor(() => expect(cull.busy).toBe(false));
    await fireEvent.click(screen.getByRole("button", { name: "Previous" }));
    await screen.findByText("Photo 1.jpg");
    expect(cull.currentID).toBe("photo-1");
    expect(document.activeElement).toBe(container.querySelector(".photo-cull"));
  },
);
