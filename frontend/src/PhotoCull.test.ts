import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
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

function preparation(targets: api.PhotoPreparedTarget[]): api.PhotoAuthoredPreparation {
  return {
    targets,
    assets: [...new Set(targets.map((target) => target.asset_id))].map((id) => ({
      id,
      revision: 1,
      kind: "photo",
      agreement: {},
      display_source: "default",
      display_file_id: targets.find((target) => target.asset_id === id)!.file_id,
      created_at: "2025-01-01T00:00:00Z",
      updated_at: "2025-01-01T00:00:00Z",
      files: targets
        .filter((target) => target.asset_id === id)
        .map((target) => ({
          ...target.values,
          id: target.file_id,
          asset_id: id,
          node_id: target.node_id,
          revision: target.revision,
          role: "image",
          created_at: "2025-01-01T00:00:00Z",
        })),
    })),
  };
}
beforeEach(() => {
  vi.spyOn(api, "preparePhotoAuthored").mockImplementation(async (scope) =>
    preparation((scope.asset_ids ?? []).map((id) => preparedPhoto(Number(id.slice(6))))),
  );
});

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
  await fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
  await screen.findByText("Photo 2.jpg");
  expect(cull.currentID).toBe(rows[1]!.asset_id);
  await fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" });
  await screen.findByText("Photo 1.jpg");
  await fireEvent.keyDown(document.activeElement!, { key: "p" });
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
  vi.spyOn(api, "preparePhotoAuthored").mockResolvedValue(preparation([before]));
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

it("shows prepared flag, rating and rotation after a no-op decision without replacing Undo", async () => {
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic-photo");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: async () => {},
  });
  const target = preparedPhoto(1);
  target.values = { ...target.values, flag: "pick", rating: 4, rotation: 90 };
  vi.mocked(api.preparePhotoAuthored).mockResolvedValue(preparation([target]));
  const edit = vi.spyOn(api, "editPhotoAuthored").mockResolvedValue({
    receipt_id: "",
    before: [],
    after: [],
  });
  const row = photo(1);
  row.previews.grid = { state: "ready", generation_id: "grid-1" };
  const { cull, photos, container } = await reviewPhoto(row);
  photos.authored.undoState.receiptID = "previous-decision";
  photos.authored.undoState.summary = "Previous edit";
  expect(screen.getByText("Unflagged · 0 stars")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
  await screen.findByText("No changes to apply");
  expect(edit).toHaveBeenCalledOnce();
  expect(edit.mock.calls[0]![0]).toMatchObject({
    targets: [{ file_id: target.file_id, revision: target.revision }],
    patch: { flag: "pick" },
  });
  await screen.findByText("pick · 4 stars");
  await waitFor(() =>
    expect(container.querySelector(".photo-stage img")?.getAttribute("style")).toContain(
      "rotate(90deg)",
    ),
  );
  expect(cull.currentID).toBe("photo-1");
  expect(photos.authored.undoState.receiptID).toBe("previous-decision");
  expect(photos.authored.undoState.summary).toBe("Previous edit");
});

it.each([false, true])(
  "restores keyboard review after preview retry: reload=%s",
  async (reload) => {
    vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic-photo");
    vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
    Object.defineProperty(HTMLImageElement.prototype, "decode", {
      configurable: true,
      value: async () => {},
    });
    const { photos, cull, container } = await reviewPhoto();
    vi.mocked(photos.preview)
      .mockRejectedValueOnce(
        reload
          ? new APIError("Display changed", 409, "photo_display_changed")
          : new Error("Preview unavailable"),
      )
      .mockResolvedValue({ state: "ready", generation_id: "fit-1" });
    const retry = await screen.findByRole("button", { name: "Retry preview" });
    retry.focus();
    await fireEvent.click(retry);
    await waitFor(() => expect(container.querySelector(".photo-stage img")).toBeTruthy());
    await waitFor(() =>
      expect(document.activeElement).toBe(container.querySelector(".photo-cull")),
    );
    await fireEvent.keyDown(document.activeElement!, { key: "a" });
    expect(cull.run!.checkpoint.autoAdvance).toBe(true);
  },
);

it("returns to fit after Z, pan, Z in Cull", async () => {
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic-photo");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: async () => {},
  });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(600);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(400);
  vi.spyOn(HTMLImageElement.prototype, "naturalWidth", "get").mockReturnValue(1200);
  vi.spyOn(HTMLImageElement.prototype, "naturalHeight", "get").mockReturnValue(800);
  const row = photo(1);
  row.previews.grid = { state: "ready", generation_id: "grid-1" };
  const { container } = await reviewPhoto(row);
  const image = await waitFor(() => {
    const image = container.querySelector<HTMLImageElement>(".photo-stage img");
    expect(image).toBeTruthy();
    return image!;
  });
  await fireEvent.load(image);
  const shell = container.querySelector(".photo-cull")!;
  const viewport = container.querySelector(".photo-viewport")!;
  const pan = container.querySelector<HTMLElement>(".photo-stage .pan")!;
  await fireEvent.keyDown(shell, { key: "z" });
  await waitFor(() => expect(pan.style.transform).toBe("translate(0px, 0px) scale(2)"));
  for (const [type, x] of [
    ["pointerdown", 100],
    ["pointermove", 250],
    ["pointerup", 250],
  ] as const) {
    const event = new MouseEvent(type, { bubbles: true, clientX: x, clientY: 100, button: 0 });
    Object.defineProperties(event, { pointerType: { value: "mouse" }, pointerId: { value: 1 } });
    await fireEvent(viewport, event);
  }
  expect(pan.style.transform).toBe("translate(150px, 0px) scale(2)");
  await fireEvent.keyDown(shell, { key: "z" });
  await waitFor(() => expect(pan.style.transform).toBe("translate(0px, 0px) scale(1)"));
});

it("refuses a decision on a replaced display file until the photo reloads", async () => {
  const row = photo(1);
  const replacement = {
    ...row,
    display_file_id: "replacement-file",
    content_version_id: "replacement-version",
  };
  const target = preparedPhoto(1, { file_id: replacement.display_file_id });
  const prepare = vi.mocked(api.preparePhotoAuthored).mockResolvedValue(preparation([target]));
  const edit = vi.spyOn(api, "editPhotoAuthored").mockResolvedValue({
    receipt_id: "reject",
    before: [target],
    after: [{ ...target, values: { ...target.values, flag: "reject" } }],
  });
  const { cull, photos, container } = await reviewPhoto(row, true);
  await fireEvent.click(screen.getByRole("button", { name: "Reject (X)" }));
  await screen.findByText("This photo changed. Reload the photo before deciding.");
  expect(prepare).toHaveBeenCalledOnce();
  expect(edit).not.toHaveBeenCalled();
  expect(cull.currentID).toBe(row.asset_id);
  expect(photos.cullEditor!.dirty).toBe(false);
  expect(container.querySelector(".photo-cull > .kit-sr-only")!.textContent).toBe("");
  vi.mocked(photos.lookup).mockResolvedValue(replacement);
  await fireEvent.click(screen.getByRole("button", { name: "Reload photo" }));
  await waitFor(() =>
    expect(screen.queryByText("This photo changed. Reload the photo before deciding.")).toBeNull(),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Reject (X)" }));
  await waitFor(() => expect(edit).toHaveBeenCalledOnce());
  expect(edit.mock.calls[0]![0]).toMatchObject({
    targets: [{ file_id: replacement.display_file_id }],
    patch: { flag: "reject" },
  });
});

it("includes the last photo picked after completion when S starts Picks", async () => {
  const row = photo(1),
    before = preparedPhoto(1);
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.spyOn(api, "preparePhotoAuthored").mockResolvedValue(preparation([before]));
  vi.spyOn(api, "editPhotoAuthored").mockImplementation(async () => {
    row.flag = "pick";
    return {
      receipt_id: "pick",
      before: [before],
      after: [{ ...before, revision: 2, values: { ...before.values, flag: "pick" } }],
    };
  });
  vi.spyOn(api, "listPhotoAssets").mockImplementation(async () => ({
    items: [{ ...row }],
    total: 1,
    facets: [],
    relevance_available: false,
  }));
  const { cull } = await reviewPhoto(row);
  await act(() => {
    cull.run = { ...cull.run!, checkpoint: { ...cull.run!.checkpoint, scope: "unflagged" } };
  });
  await fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
  await screen.findByRole("heading", { name: "Pass complete" });
  await waitFor(() => expect(cull.busy).toBe(false));
  await fireEvent.click(screen.getByRole("button", { name: "Previous" }));
  await screen.findByText(row.name);
  await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
  await waitFor(() => expect(screen.getByText("pick · 0 stars")).toBeTruthy());
  await fireEvent.keyDown(document.activeElement!, { key: "s" });
  await screen.findByRole("button", { name: "Scope: picks" });
  expect(cull.run!.checkpoint.pass).toEqual([row.asset_id]);
  expect(screen.queryByRole("heading", { name: "Pass complete" })).toBeNull();
});

it("counts a captured photo lost before the completion refresh as unavailable", async () => {
  const row = photo(1);
  vi.spyOn(api, "listPhotoAssets").mockResolvedValue({
    items: [row],
    total: 1,
    facets: [],
    relevance_available: false,
  });
  const { cull } = await reviewPhoto(row);
  await act(() => {
    cull.run = { ...cull.run!, ids: [row.asset_id, photo(2).asset_id] };
  });
  await fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
  await screen.findByRole("heading", { name: "Pass complete" });
  await screen.findByText(/1 unavailable/);
  expect(cull.run!.checkpoint.unavailable).toEqual([photo(2).asset_id]);
});

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

it("requires reloading the photo when a 412 retry prepares a different display file", async () => {
  const row = photo(1);
  const before = preparedPhoto(1);
  const other = preparedPhoto(1, { file_id: "other-file", file_name: "Other.jpg" });
  const initial = preparation([before, other]);
  const reloaded = preparation([
    { ...before, revision: 2 },
    { ...other, revision: 2 },
  ]);
  reloaded.assets[0]!.display_file_id = other.file_id;
  const prepare = vi
    .mocked(api.preparePhotoAuthored)
    .mockResolvedValueOnce(initial)
    .mockResolvedValue(reloaded);
  const edit = vi
    .spyOn(api, "editPhotoAuthored")
    .mockRejectedValueOnce(new APIError("Photo changed", 412, "precondition_failed"))
    .mockResolvedValue({ receipt_id: "pick", before: [], after: [] });
  const { cull, photos } = await reviewPhoto(row);
  await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
  await screen.findByText("Photo changed");
  expect(edit).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: "Reload and retry" }));
  await waitFor(() => expect(prepare).toHaveBeenCalledTimes(2));
  await waitFor(() => expect(photos.cullEditor!.busy).toBe(false));
  expect(edit).toHaveBeenCalledOnce();
  expect(screen.getByRole("button", { name: "Reload photo" })).toBeTruthy();
  expect(cull.currentID).toBe(row.asset_id);
});

it.each([false, true])(
  "reloads a 412 and retries the kept decision after member review=%s",
  async (membershipChanged) => {
    const before = preparedPhoto(1),
      extra = preparedPhoto(1, { file_id: "other-file", file_name: "Other.jpg" });
    const prepare = vi
      .spyOn(api, "preparePhotoAuthored")
      .mockResolvedValueOnce(preparation(membershipChanged ? [before, extra] : [before]))
      .mockResolvedValue(preparation([{ ...before, revision: 2 }]));
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
