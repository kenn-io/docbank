import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { PhotoAuthoredEditor } from "./photoAuthored.svelte.js";
import { APIError } from "./api-transport.js";
import PhotoCullView from "./PhotoCull.svelte";
import { PhotoCull } from "./photoCull.svelte.js";
import { Photos } from "./photos.svelte.js";
import { photo, preparedPhoto } from "./photo-test-fixtures.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";
import { photoCullStorage, type CullRun } from "./photoCullStorage.js";
import { IDBFactory, IDBKeyRange } from "fake-indexeddb";
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
async function readyButton(name: string | RegExp) {
  const button = await screen.findByRole("button", { name });
  await waitFor(() => expect(button.matches(":disabled")).toBe(false));
  return button;
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
    saved: vi.fn(async () => cull.run?.checkpoint),
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
  await fireEvent.click(await readyButton(/^Undo$/));
  expect(undo).toHaveBeenCalled();
  await screen.findByText("Photo 2.jpg");
  await fireEvent.keyDown(document.querySelector(".photo-cull")!, { key: "0" });
  await waitFor(() => expect(edit).toHaveBeenCalledTimes(2));
  expect(edit.mock.calls[1]?.slice(0, 3)).toEqual([
    { asset_ids: [rows[1]!.asset_id] },
    { rating: 0 },
    false,
  ]);
  await readyButton("Exit cull");
  await fireEvent.keyDown(document.querySelector(".photo-cull")!, { key: "Escape" });
  expect(close).toHaveBeenCalledOnce();
});
it("navigates with keyboard focus after activating Undo", async () => {
  const before = preparedPhoto(1);
  const picked = { ...before, revision: 2, values: { ...before.values, flag: "pick" as const } };
  vi.spyOn(api, "editPhotoAuthored").mockResolvedValue({
    receipt_id: "pick",
    before: [before],
    after: [picked],
  });
  const undo = vi.spyOn(api, "undoPhotoAuthored").mockResolvedValue({
    receipt_id: "undo",
    before: [picked],
    after: [{ ...before, revision: 3 }],
  });
  const rows = [photo(1), photo(2), photo(3)];
  const { cull, photos } = await reviewPhoto(rows[0], true);
  vi.spyOn(photos, "metadataChanged").mockReturnValue(new Map());
  vi.mocked(photos.lookup).mockImplementation(
    async (id) => rows.find((row) => row.asset_id === id)!,
  );
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: rows.map((row) => row.asset_id),
      checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
    };
  });
  await fireEvent.click(await readyButton("Pick (P)"));
  await screen.findByText("Photo 2.jpg");
  const button = await readyButton(/^Undo$/);
  button.focus();
  expect(document.activeElement).toBe(button);
  await fireEvent.click(button);
  await waitFor(() => expect(screen.queryByRole("button", { name: /^Undo$/ })).toBeNull());
  expect(undo).toHaveBeenCalledWith("pick", expect.anything());
  await readyButton("Next");
  await fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
  await waitFor(() => expect(cull.currentID).toBe("photo-3"));
  await screen.findByText("Photo 3.jpg");
});

it("blocks navigation during preparation and the write", async () => {
  let finishPrepare!: (value: api.PhotoAuthoredPreparation) => void;
  const pendingPrepare = new Promise<api.PhotoAuthoredPreparation>(
    (resolve) => (finishPrepare = resolve),
  );
  vi.mocked(api.preparePhotoAuthored).mockReturnValue(pendingPrepare);
  let finishWrite!: (value: PhotoAuthoredReceipt) => void;
  const pendingWrite = new Promise<PhotoAuthoredReceipt>((resolve) => (finishWrite = resolve));
  const edit = vi.spyOn(api, "editPhotoAuthored").mockReturnValue(pendingWrite);
  const { cull, photos, container } = await reviewPhoto();
  const shell = container.querySelector(".photo-cull")!;
  await fireEvent.keyDown(shell, { key: "p" });
  for (const phase of ["prepare", "write"]) {
    expect(photos.cullEditor!.canChangeTarget()).toBe(false);
    await fireEvent.keyDown(shell, { key: "ArrowRight" });
    await fireEvent.keyDown(shell, { key: "Escape" });
    expect(cull.currentID).toBe("photo-1");
    expect(screen.getByRole("button", { name: "Exit cull" }).matches(":disabled")).toBe(true);
    if (phase === "prepare") {
      await act(() => finishPrepare(preparation([preparedPhoto(1)])));
      await waitFor(() => expect(edit).toHaveBeenCalledOnce());
    }
  }
  await act(() => finishWrite({ receipt_id: "", before: [], after: [] }));
  await readyButton("Exit cull");
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
    saved: vi.fn(async () => cull.run?.checkpoint),
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
    saved: vi.fn(async () => cull.run?.checkpoint),
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

it("disables auto-advance changes while a decision is pending", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const rows = [photo(1), photo(2)];
  const cull = new PhotoCull("session", {
    saved: vi.fn(async () => cull.run?.checkpoint),
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
  let save!: (receipt: PhotoAuthoredReceipt) => void;
  const saving = new Promise<PhotoAuthoredReceipt>((resolve) => (save = resolve));
  const edit = vi.spyOn(api, "editPhotoAuthored").mockReturnValue(saving);
  render(PhotoCullView, {
    cull,
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    onclose: vi.fn(),
  });
  await screen.findByText("Photo 1.jpg");
  const shell = document.querySelector(".photo-cull")!;
  await fireEvent.keyDown(shell, { key: "p" });
  await waitFor(() => expect(edit).toHaveBeenCalledOnce());
  const toggle = screen.getByRole("button", { name: "Auto-advance: on" });
  expect(toggle.matches(":disabled")).toBe(true);
  await act(() => (toggle as HTMLButtonElement).click());
  await fireEvent.keyDown(shell, { key: "a" });
  expect(cull.run!.checkpoint.autoAdvance).toBe(true);
  expect(toggle.textContent).toContain("Auto-advance: on");
  expect(cull.currentID).toBe(rows[0]!.asset_id);
  await act(async () => {
    save({ receipt_id: "pick", before: [preparedPhoto(1)], after: [] });
    await saving;
  });
  await screen.findByText("Photo 2.jpg");
  expect(document.querySelector(".kit-sr-only")?.textContent).toContain("Photo 1.jpg: pick");
  expect(cull.currentID).toBe(rows[1]!.asset_id);
  await readyButton("Auto-advance: on");
});

it("retries a failed lookup without leaving the current photo", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const row = photo(1);
  const cull = new PhotoCull("session", {
    saved: vi.fn(async () => cull.run?.checkpoint),
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
      autoAdvance: true,
    },
  };
  const photos = new Photos("session", vi.fn());
  const lookup = vi
    .spyOn(photos, "lookup")
    .mockRejectedValueOnce(new APIError("Temporarily unavailable", 503, "unavailable"))
    .mockResolvedValueOnce(row);
  vi.spyOn(photos, "lookupMany").mockResolvedValue([]);
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  render(PhotoCullView, {
    cull,
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    onclose: vi.fn(),
  });
  await fireEvent.click(await screen.findByRole("button", { name: /^Retry$/ }));
  await screen.findByText(row.name);
  expect(lookup).toHaveBeenCalledTimes(2);
  expect(lookup.mock.calls.map(([id]) => id)).toEqual([row.asset_id, row.asset_id]);
  expect(cull.currentID).toBe(row.asset_id);
  expect(cull.run!.checkpoint.position).toBe(0);
  expect(screen.queryByRole("button", { name: /^Retry$/ })).toBeNull();
});

async function reviewPhoto(
  row = photo(1),
  autoAdvance = false,
  store?: ReturnType<typeof photoCullStorage>,
) {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const cull = new PhotoCull(
    "session",
    store ?? {
      saved: vi.fn(async () => cull.run?.checkpoint),
      load: vi.fn(),
      replace: vi.fn(),
      checkpoint: vi.fn(),
    },
  );
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

it("reloads stale content after a 412 without reapplying the decision", async () => {
  const row = photo(1);
  const edit = vi
    .spyOn(api, "editPhotoAuthored")
    .mockRejectedValue(new APIError("Photo changed", 412, "precondition_failed"));
  const { cull, photos } = await reviewPhoto(row);
  await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
  await screen.findByText("Photo changed");
  expect(edit).toHaveBeenCalledOnce();
  expect(edit.mock.calls[0]![0]).toMatchObject({
    targets: [{ file_id: row.display_file_id, content_version_id: row.content_version_id }],
    patch: { flag: "pick" },
  });
  vi.mocked(photos.lookup).mockResolvedValue({
    ...row,
    name: "Replacement.jpg",
    content_version_id: "replacement-version",
  });
  await fireEvent.click(await readyButton("Reload photo"));
  await screen.findByText("Replacement.jpg");
  expect(edit).toHaveBeenCalledOnce();
  expect(photos.cullEditor!.dirty).toBe(false);
  expect(photos.cullEditor!.conflict).toBe(false);
  expect(cull.currentID).toBe(row.asset_id);
});

it.each([
  ["network", new TypeError("Failed to fetch")],
  ["503", new APIError("Service unavailable", 503, "unavailable")],
])("retries a %s failure with the viewed content binding", async (_kind, cause) => {
  const edit = vi
    .spyOn(api, "editPhotoAuthored")
    .mockRejectedValueOnce(cause)
    .mockResolvedValue({ receipt_id: "", before: [], after: [] });
  const { photos } = await reviewPhoto();
  await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
  await screen.findByText(cause.message);
  await fireEvent.click(await readyButton("Retry edit"));
  await screen.findByText("No changes to apply");
  expect(edit).toHaveBeenCalledTimes(2);
  expect(edit.mock.calls[1]![0]).toEqual(edit.mock.calls[0]![0]);
  expect(edit.mock.calls[1]![0].targets[0]).toMatchObject({
    file_id: "file-1",
    content_version_id: "version-1",
  });
  expect(photos.lookup).toHaveBeenCalledOnce();
});

it("keeps rail thumbnails while loading and shows rail errors", async () => {
  const { cull, photos } = await reviewPhoto();
  await screen.findByRole("button", { name: "Show Photo 1.jpg" });
  let fail!: (cause: Error) => void;
  vi.mocked(photos.lookupMany).mockReturnValue(new Promise((_, reject) => (fail = reject)));
  await act(() => {
    cull.run = {
      ...cull.run!,
      checkpoint: { ...cull.run!.checkpoint, pass: ["photo-1", "photo-2"] },
    };
  });
  expect(screen.getByRole("button", { name: "Show Photo 1.jpg" })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Unavailable" })).toBeNull();
  await act(() => fail(new Error("Filmstrip unavailable")));
  await screen.findByRole("alert");
  expect(screen.getByText("Filmstrip unavailable")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Show Photo 1.jpg" })).toBeTruthy();
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
  await readyButton("Scope: unflagged");
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

it("refuses a stale tab decision before calling the edit API", async () => {
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("IDBKeyRange", IDBKeyRange);
  const store = photoCullStorage("synthetic-vault");
  const { cull } = await reviewPhoto(photo(1), false, store);
  await store.replace(cull.run!);
  const other = new PhotoCull("session", photoCullStorage("synthetic-vault"));
  await other.restore();
  other.move(1);
  await other.flush();
  const edit = vi.spyOn(api, "editPhotoAuthored");
  await fireEvent.click(screen.getByRole("button", { name: "Reject (X)" }));
  await screen.findByText(
    "Progress could not be saved: Cull progress changed in another tab. Close and resume it.",
  );
  expect(cull.stale).toBe(true);
  expect(edit).not.toHaveBeenCalled();
});

it("shows the navigation blocker after a failed decision", async () => {
  vi.spyOn(api, "editPhotoAuthored").mockRejectedValue(new Error("Decision failed"));
  const { cull } = await reviewPhoto();
  await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
  await screen.findByText("Decision failed");
  await fireEvent.keyDown(document.querySelector(".photo-cull")!, { key: "Escape" });
  expect(screen.getByRole("alert").textContent).toContain(
    "Apply or clear your metadata edits before changing photos.",
  );
  expect(cull.currentID).toBe("photo-1");
});
