import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { APIError } from "./api-transport.js";
import PhotoCullView from "./PhotoCull.svelte";
import PhotoCullConfirmation from "./PhotoCullConfirmation.svelte";
import { PhotoCull } from "./photoCull.svelte.js";
import { Photos } from "./photos.svelte.js";
import { HiddenAccess } from "./hiddenAccess.svelte.js";
import { photo, photoPreparation, preparedPhoto } from "./photo-test-fixtures.js";
import type { PhotoBrowseRow } from "./generated/docbank.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";
import { photoCullStorage, type CullRun } from "./photoCullStorage.js";
import { IDBFactory, IDBKeyRange } from "fake-indexeddb";
import * as api from "./generated/docbank.js";
import type { PhotoAuthoredReceipt } from "./generated/docbank.js";

async function readyButton(name: string | RegExp) {
  const button = await screen.findByRole("button", { name });
  await waitFor(() => expect(button.matches(":disabled")).toBe(false));
  return button;
}
beforeEach(() => {
  vi.spyOn(api, "getPhotoSettings").mockResolvedValue({ revision: 1, burst_gap_seconds: 2 });
  vi.spyOn(api, "groupPhotoStacks").mockResolvedValue({
    groups: [],
    unavailable_ids: [],
    burst_gap_seconds: 2,
  });
  vi.spyOn(api, "preparePhotoAuthored").mockImplementation(async (scope) =>
    photoPreparation((scope.asset_ids ?? []).map((id) => preparedPhoto(Number(id.slice(6))))),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  delete (HTMLImageElement.prototype as { decode?: unknown }).decode;
  vi.unstubAllGlobals();
});
it.each(
  [
    { key: "0", patch: { rating: 0 }, announcement: "0 stars", next: "photo-1" },
    { key: "4", patch: { rating: 4 }, announcement: "4 stars", next: "photo-1" },
    { key: "5", patch: { rating: 5 }, announcement: "5 stars", next: "photo-1" },
    { key: "p", patch: { flag: "pick" }, announcement: "pick", next: "photo-2" },
    { key: "x", patch: { flag: "reject" }, announcement: "reject", next: "photo-2" },
    { key: "u", patch: { flag: "" }, announcement: "flag cleared", next: "photo-2" },
  ].flatMap((decision) =>
    (decision.key === "p" ? [false, true] : [false]).map((retry) => ({ ...decision, retry })),
  ),
)(
  "auto-advance follows flag decisions for $key with retry=$retry",
  async ({ key, patch, announcement, next, retry }) => {
    const edit = vi
      .spyOn(api, "editPhotoAuthored")
      .mockResolvedValue({ receipt_id: "", before: [], after: [] });
    if (retry) edit.mockRejectedValueOnce(new APIError("Service unavailable", 503, "unavailable"));
    const rows = [photo(1), photo(2)];
    const { cull, photos, container } = await reviewPhoto(rows[0], true);
    vi.mocked(photos.lookup).mockImplementation(async (id) =>
      rows.find((row) => row.asset_id === id)!,
    );
    await act(() => {
      cull.run = {
        ...cull.run!,
        ids: rows.map((row) => row.asset_id),
        checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
      };
    });
    await fireEvent.keyDown(container.querySelector(".photo-cull")!, { key });
    if (retry) {
      await screen.findByText("Service unavailable");
      expect(cull.currentID).toBe("photo-1");
      await fireEvent.click(await readyButton("Retry edit"));
    }
    await waitFor(() =>
      expect(container.querySelector(".photo-cull > span[aria-live='polite']")?.textContent).toBe(
        `Photo 1.jpg: ${announcement}`,
      ),
    );
    expect(edit).toHaveBeenCalledTimes(retry ? 2 : 1);
    for (const [request] of edit.mock.calls) expect(request.patch).toEqual(patch);
    expect(cull.currentID).toBe(next);
    await screen.findByText(next === "photo-1" ? "Photo 1.jpg" : "Photo 2.jpg");
  },
);

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
  const { cull, photos, onclose } = await reviewPhoto(rows[0], true);
  vi.spyOn(photos, "metadataChanged").mockReturnValue(new Map());
  vi.mocked(photos.lookup).mockImplementation(async (id) =>
    rows.find((row) => row.asset_id === id)!,
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
  await waitFor(() => expect(undo).toHaveBeenCalledWith("pick", expect.anything()));
  await waitFor(() => expect(screen.queryByRole("button", { name: /^Undo$/ })).toBeNull());
  await readyButton("Next");
  await fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
  await waitFor(() => expect(cull.currentID).toBe("photo-3"));
  await screen.findByText("Photo 3.jpg");
  await readyButton("Exit cull");
  await fireEvent.keyDown(document.querySelector(".photo-cull")!, { key: "Escape" });
  expect(onclose).toHaveBeenCalledOnce();
});

it("blocks decisions, navigation, scope changes and exit during checkpoint checks, preparation and writes", async () => {
  let finishCheck!: (value: CullRun["checkpoint"]) => void;
  const saved = vi.fn(
    () => new Promise<CullRun["checkpoint"]>((resolve) => (finishCheck = resolve)),
  );
  let finishPrepare!: (value: api.PhotoAuthoredPreparation) => void;
  const pendingPrepare = new Promise<api.PhotoAuthoredPreparation>(
    (resolve) => (finishPrepare = resolve),
  );
  vi.mocked(api.preparePhotoAuthored).mockReturnValue(pendingPrepare);
  let finishWrite!: (value: PhotoAuthoredReceipt) => void;
  const pendingWrite = new Promise<PhotoAuthoredReceipt>((resolve) => (finishWrite = resolve));
  const edit = vi.spyOn(api, "editPhotoAuthored").mockReturnValue(pendingWrite);
  const rows = [photo(1), photo(2), photo(3)];
  const { cull, photos, container, onclose } = await reviewPhoto(rows[0], false, {
    saved,
    load: vi.fn(),
    replace: vi.fn(),
    checkpoint: vi.fn(),
  });
  vi.mocked(photos.lookupMany).mockImplementation(async (ids) =>
    rows.filter((row) => ids.includes(row.asset_id)),
  );
  vi.mocked(photos.lookup).mockImplementation(async (id) =>
    rows.find((row) => row.asset_id === id)!,
  );
  await act(() => {
    cull.run = {
      ids: rows.map((row) => row.asset_id),
      checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id), position: 1 },
    };
  });
  await screen.findByText("Photo 2.jpg");
  const move = vi.spyOn(cull, "move");
  const choose = vi.spyOn(cull, "choose");
  const skip = vi.spyOn(cull, "skip");
  const nextPass = vi.spyOn(cull, "nextPass");
  const shell = container.querySelector(".photo-cull")!;
  await fireEvent.keyDown(shell, { key: "p" });
  for (const phase of ["check", "prepare", "recheck", "write"]) {
    await waitFor(() => {
      if (phase === "check") expect(saved).toHaveBeenCalledOnce();
      else if (phase === "prepare") expect(photos.cullEditor!.loading).toBe(true);
      else if (phase === "recheck") expect(saved).toHaveBeenCalledTimes(2);
      else expect(photos.cullEditor!.busy).toBe(true);
    });
    for (const key of [
      "ArrowLeft",
      "ArrowUp",
      "ArrowRight",
      "ArrowDown",
      "Escape",
      "s",
      "a",
      "p",
      "x",
      "u",
      "4",
    ])
      await fireEvent.keyDown(shell, { key });
    for (const name of [
      "Previous",
      "Next",
      "Skip",
      "Exit cull",
      "Scope: all",
      "Pick (P)",
      "Reject (X)",
      "Clear flag (U)",
      "Show Photo 1.jpg",
      "Show Photo 3.jpg",
    ]) {
      const button = screen.getByRole("button", { name });
      expect(button.matches(":disabled")).toBe(true);
      await fireEvent.click(button);
    }
    // The toggle has no handler guard, so only a click that honors disabled proves it is blocked.
    const toggle = screen.getByRole("button", { name: "Auto-advance: off" }) as HTMLButtonElement;
    expect(toggle.matches(":disabled")).toBe(true);
    await act(() => toggle.click());
    expect(cull.currentID).toBe("photo-2");
    expect(cull.run!.checkpoint.autoAdvance).toBe(false);
    expect(move).not.toHaveBeenCalled();
    expect(choose).not.toHaveBeenCalled();
    expect(skip).not.toHaveBeenCalled();
    expect(nextPass).not.toHaveBeenCalled();
    expect(onclose).not.toHaveBeenCalled();
    expect(saved).toHaveBeenCalledTimes(phase === "check" || phase === "prepare" ? 1 : 2);
    if (phase === "check" || phase === "recheck") {
      if (phase === "check") expect(api.preparePhotoAuthored).not.toHaveBeenCalled();
      else expect(edit).not.toHaveBeenCalled();
      await act(() => finishCheck(cull.run!.checkpoint));
      if (phase === "recheck") await waitFor(() => expect(edit).toHaveBeenCalledOnce());
    } else if (phase === "prepare") {
      expect(edit).not.toHaveBeenCalled();
      await act(() => finishPrepare(photoPreparation([preparedPhoto(2)])));
    }
  }
  await act(() => finishWrite({ receipt_id: "", before: [], after: [] }));
  await readyButton("Exit cull");
  await fireEvent.click(screen.getByRole("button", { name: "Next" }));
  expect(cull.currentID).toBe("photo-3");
  await fireEvent.click(screen.getByRole("button", { name: "Exit cull" }));
  expect(onclose).toHaveBeenCalledOnce();
});

it("retries a failed completion lookup and restores the unflagged remainder offer", async () => {
  const lookup = vi
    .spyOn(api, "lookupPhotoFlags")
    .mockRejectedValueOnce(new APIError("Service unavailable", 503, "unavailable"))
    .mockResolvedValue({ items: [photo(1)] });
  const { cull } = await reviewPhoto();
  await fireEvent.click(await readyButton("Next"));
  await screen.findByRole("heading", { name: "Pass complete" });
  await screen.findByText("Service unavailable");
  expect(screen.queryByRole("button", { name: "Review unflagged remainder" })).toBeNull();
  await fireEvent.click(await readyButton(/^Retry$/));
  await readyButton("Review unflagged remainder");
  expect(lookup).toHaveBeenCalledTimes(2);
  expect(screen.queryByText("Service unavailable")).toBeNull();
  expect(screen.queryByRole("button", { name: /^Retry$/ })).toBeNull();
  expect(cull.complete).toBe(true);
});

it("retries a failed lookup without leaving the current photo", async () => {
  const row = photo(1);
  const { cull, photos } = await reviewPhoto(row, true, undefined, {
    lookupError: new APIError("Temporarily unavailable", 503, "unavailable"),
  });
  const lookup = vi.mocked(photos.lookup);
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
  options: {
    lookupError?: Error;
    hiddenAccess?: HiddenAccess;
    onmetadatachanged?: () => void;
  } = {},
) {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const cull: PhotoCull = new PhotoCull(
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
  const photos = new Photos("session", vi.fn(), false, undefined, {
    hiddenAccess: options.hiddenAccess,
  });
  const lookup = vi.spyOn(photos, "lookup").mockResolvedValue(row);
  if (options.lookupError) lookup.mockRejectedValueOnce(options.lookupError);
  vi.spyOn(photos, "lookupMany").mockResolvedValue([row]);
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  const onclose = vi.fn();
  const view = render(PhotoCullView, {
    cull,
    photos,
    cache: {
      get: vi.fn(async (_asset: string, id: string) =>
        Object.assign(new Blob(["synthetic-photo"]), { id }),
      ),
    } as unknown as PhotoPreviewCache,
    onclose,
    onmetadatachanged: options.onmetadatachanged,
  });
  if (options.lookupError) await screen.findByText(options.lookupError.message);
  else await screen.findByText(row.name);
  return { cull, photos, onclose, ...view };
}

it("answers false when Escape dismisses a confirmation", async () => {
  const { component } = render(PhotoCullConfirmation);
  const answer = component.confirm("Reset this pass?", "Reset pass");
  const dialog = await screen.findByRole("dialog", { name: "Reset this pass?" });
  await fireEvent.keyDown(dialog, { key: "Escape" });
  expect(await answer).toBe(false);
  expect(dialog.isConnected).toBe(false);
});

it("Escape cancels only the scope confirmation and allows a later S", async () => {
  const { cull, container, onclose } = await reviewPhoto();
  const checkpoint = cull.run!.checkpoint;
  const nextPass = vi.spyOn(cull, "nextPass");
  const shell = container.querySelector(".photo-cull")!;
  await fireEvent.keyDown(shell, { key: "s" });
  const dialog = await screen.findByRole("dialog", {
    name: "Change scope and reset this pass's progress?",
  });
  await fireEvent.keyDown(dialog, { key: "Escape" });
  await waitFor(() => expect(dialog.isConnected).toBe(false));
  await nextPass.mock.results[0]!.value;
  expect(cull.run!.checkpoint).toBe(checkpoint);
  expect(onclose).not.toHaveBeenCalled();
  expect(screen.getByRole("dialog", { name: "Cull photos" })).toBeTruthy();
  await fireEvent.keyDown(shell, { key: "s" });
  await screen.findByRole("dialog", { name: "Change scope and reset this pass's progress?" });
});

it("refreshes current and cached rows when a new pass starts on the same photo", async () => {
  const { cull, photos } = await reviewPhoto();
  const rows = [photo(1), photo(2)];
  vi.mocked(photos.lookup).mockImplementation(async (id) =>
    rows.find((row) => row.asset_id === id)!,
  );
  vi.mocked(photos.lookupMany).mockImplementation(async (ids) =>
    rows.filter((row) => ids.includes(row.asset_id)),
  );
  await act(() => {
    cull.run = {
      ids: rows.map((row) => row.asset_id),
      checkpoint: {
        ...cull.run!.checkpoint,
        scope: "unflagged",
        pass: rows.map((row) => row.asset_id),
      },
    };
  });
  await screen.findByRole("button", { name: "Show Photo 2.jpg" });
  await screen.findByText("Unflagged · 0 stars");
  rows[0] = { ...rows[0]!, flag: "pick" };
  rows[1] = { ...rows[1]!, flag: "pick" };
  vi.spyOn(api, "lookupPhotoFlags").mockResolvedValue({
    items: rows,
  });
  await fireEvent.click(await readyButton("Scope: unflagged"));
  await fireEvent.click(screen.getByRole("button", { name: "Reset pass" }));
  await readyButton("Scope: picks");
  expect(cull.currentID).toBe("photo-1");
  expect(cull.run!.checkpoint.autoAdvance).toBe(false);
  await screen.findByText("pick · 0 stars");
  await fireEvent.click(await readyButton("Next"));
  await screen.findByText("Photo 2.jpg");
  await screen.findByText("pick · 0 stars");
});

it("refuses a decision after another client changes the cached neighbor flag", async () => {
  const edit = vi.spyOn(api, "editPhotoAuthored").mockResolvedValue({
    receipt_id: "",
    before: [],
    after: [],
  });
  const { cull, photos, container } = await reviewPhoto();
  vi.mocked(photos.lookupMany).mockResolvedValue([photo(2)]);
  await act(() => {
    cull.run = {
      ids: ["photo-1", "photo-2"],
      checkpoint: { ...cull.run!.checkpoint, pass: ["photo-1", "photo-2"] },
    };
  });
  await screen.findByRole("button", { name: "Show Photo 2.jpg" });
  const lookups = vi.mocked(photos.lookup).mock.calls.length;
  await fireEvent.click(await readyButton("Next"));
  await screen.findByText("Photo 2.jpg");
  expect(photos.lookup).toHaveBeenCalledTimes(lookups);
  await screen.findByText("Unflagged · 0 stars");
  const target = preparedPhoto(2);
  vi.mocked(api.preparePhotoAuthored).mockResolvedValue(
    photoPreparation([{ ...target, revision: 2, values: { ...target.values, flag: "pick" } }]),
  );
  await fireEvent.keyDown(container.querySelector(".photo-cull")!, { key: "x" });
  await screen.findByRole("button", { name: "Reload photo" });
  expect(photos.cullEditor!.conflict).toBe(true);
  expect(edit).not.toHaveBeenCalled();
  expect(cull.currentID).toBe("photo-2");
});

it.each([
  {
    name: "missing viewed file",
    prepared: [preparedPhoto(1, { file_id: "raw-1", file_name: "Photo 1.ARW" })],
  },
  {
    name: "rating differs with the same flag",
    prepared: [preparedPhoto(1, { values: { ...preparedPhoto(1).values, rating: 4 } })],
  },
  {
    name: "displayed JPEG flag in a pair",
    prepared: [
      preparedPhoto(1, { file_id: "raw-1", file_name: "Photo 1.ARW" }),
      preparedPhoto(1, { values: { ...preparedPhoto(1).values, flag: "pick" } }),
    ],
  },
])("refuses a viewed cull decision before staging: $name", async ({ prepared }) => {
  vi.mocked(api.preparePhotoAuthored).mockResolvedValue(photoPreparation(prepared));
  const edit = vi
    .spyOn(api, "editPhotoAuthored")
    .mockResolvedValue({ receipt_id: "", before: [], after: [] });
  const { cull, photos } = await reviewPhoto();
  await fireEvent.click(await readyButton("Pick (P)"));
  await screen.findByText("This photo changed. Reload the photo before deciding.");
  expect(edit).not.toHaveBeenCalled();
  expect(photos.cullEditor!.dirty).toBe(false);
  expect(cull.currentID).toBe("photo-1");
});

it("allows another decision after reconciling its own flag and rating with auto-advance off", async () => {
  const target = preparedPhoto(1);
  const edit = vi.spyOn(api, "editPhotoAuthored").mockImplementation(async (request) => {
    const before = { ...target, values: { ...target.values } };
    target.values = { ...target.values, ...request.patch } as api.PhotoAuthored;
    target.revision++;
    return { receipt_id: "decision", before: [before], after: [target] };
  });
  vi.mocked(api.preparePhotoAuthored).mockImplementation(async () =>
    photoPreparation([{ ...target, values: { ...target.values } }]),
  );
  const { cull, photos, container } = await reviewPhoto();
  vi.spyOn(photos, "metadataChanged").mockReturnValue(new Map());
  await fireEvent.click(await readyButton("Pick (P)"));
  await screen.findByText("pick · 0 stars");
  await fireEvent.keyDown(container.querySelector(".photo-cull")!, { key: "4" });
  await screen.findByText("pick · 4 stars");
  await fireEvent.click(await readyButton("Reject (X)"));
  await screen.findByText("reject · 4 stars");
  expect(edit).toHaveBeenCalledTimes(3);
  expect(cull.currentID).toBe("photo-1");
  expect(photos.cullEditor!.conflict).toBe(false);
});

it("shows a background import's rating, rotation, and flag and tells the app", async () => {
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic-photo");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: async () => {},
  });
  const target = preparedPhoto(1);
  vi.mocked(api.preparePhotoAuthored).mockImplementation(async () =>
    photoPreparation([{ ...target, values: { ...target.values } }]),
  );
  const row = photo(1);
  row.previews.grid = { state: "ready", generation_id: "grid-1" };
  const onmetadatachanged = vi.fn();
  const { cull, photos, container } = await reviewPhoto(row, false, undefined, {
    onmetadatachanged,
  });
  const refresh = vi.spyOn(photos, "refresh").mockResolvedValue(true);
  expect(screen.getByText("Unflagged · 0 stars")).toBeTruthy();
  const flagsChanged = vi.spyOn(cull, "flagsChanged");
  // The sync worker imports a sidecar edit made outside Docbank.
  target.values = { ...target.values, rating: 4, flag: "pick", rotation: 90 };
  target.revision++;
  target.node_revision++;
  target.xmp = { state: "synced", import_receipt_id: "xmp-import" };
  vi.mocked(photos.lookupMany).mockResolvedValue([
    { ...row, rating: 4, flag: "pick", rotation: 90 },
  ]);
  const tags = [
    { id: "later-page", name: "Travel", revision: 2, assignment_count: 1 },
  ] as api.Tag[];
  await photos.cullEditor!.onXMPChanged!(undefined, tags);
  await screen.findByText("pick · 4 stars");
  await waitFor(() =>
    expect(container.querySelector(".photo-stage img")?.getAttribute("style")).toContain(
      "rotate(90deg)",
    ),
  );
  expect(flagsChanged).toHaveBeenCalled();
  expect(refresh).toHaveBeenCalled();
  expect(onmetadatachanged).toHaveBeenCalledWith(undefined, tags);
  // A slower import lookup answering after a newer live reread leaves the newer values.
  let finish!: (rows: PhotoBrowseRow[]) => void;
  vi.mocked(photos.lookupMany).mockImplementationOnce(
    () => new Promise((resolve) => (finish = resolve)),
  );
  const importing = photos.cullEditor!.onXMPChanged!(undefined, []);
  await waitFor(() => expect(finish).toBeDefined());
  vi.mocked(photos.lookupMany).mockResolvedValueOnce([
    { ...row, rating: 5, flag: "reject", rotation: 90 },
  ]);
  await act(() => photos.changed({}));
  await screen.findByText("reject · 5 stars");
  await act(async () => {
    finish([{ ...row, rating: 2, flag: "pick", rotation: 90 }]);
    await importing;
  });
  expect(screen.getByText("reject · 5 stars")).toBeTruthy();
  expect(screen.queryByText("pick · 2 stars")).toBeNull();
});

it("shows prepared rotation after a no-op decision without replacing Undo", async () => {
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic-photo");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: async () => {},
  });
  const target = preparedPhoto(1);
  target.values = { ...target.values, flag: "pick", rating: 4, rotation: 90 };
  vi.mocked(api.preparePhotoAuthored).mockResolvedValue(photoPreparation([target]));
  const edit = vi.spyOn(api, "editPhotoAuthored").mockResolvedValue({
    receipt_id: "",
    before: [],
    after: [],
  });
  const row = photo(1);
  row.flag = "pick";
  row.rating = 4;
  row.previews.grid = { state: "ready", generation_id: "grid-1" };
  const { cull, photos, container } = await reviewPhoto(row);
  photos.authored.undoState.receiptID = "previous-decision";
  photos.authored.undoState.summary = "Previous edit";
  expect(screen.getByText("pick · 4 stars")).toBeTruthy();
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

it("restores keyboard review after preview retry", async () => {
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic-photo");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: async () => {},
  });
  const { photos, cull, container } = await reviewPhoto();
  vi.mocked(photos.preview)
    .mockRejectedValueOnce(new Error("Preview unavailable"))
    .mockResolvedValue({ state: "ready", generation_id: "fit-1" });
  const retry = await screen.findByRole("button", { name: "Retry preview" });
  retry.focus();
  await fireEvent.click(retry);
  await waitFor(() => expect(container.querySelector(".photo-stage img")).toBeTruthy());
  await waitFor(() => expect(document.activeElement).toBe(container.querySelector(".photo-cull")));
  await fireEvent.keyDown(document.activeElement!, { key: "a" });
  expect(cull.run!.checkpoint.autoAdvance).toBe(true);
});

it("toggles actual size with Z in Cull", async () => {
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
  const pan = container.querySelector<HTMLElement>(".photo-stage .pan")!;
  await fireEvent.keyDown(container.querySelector(".photo-cull")!, { key: "z" });
  await waitFor(() => expect(pan.style.transform).toBe("translate(0px, 0px) scale(2)"));
});

it("reloads stale content after a 412 without reapplying the decision", async () => {
  const row = photo(1);
  const edit = vi
    .spyOn(api, "editPhotoAuthored")
    .mockRejectedValue(new APIError("Photo changed", 412, "precondition_failed"));
  const { cull, photos, container } = await reviewPhoto(row);
  vi.mocked(photos.lookupMany).mockResolvedValue([photo(2)]);
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: [row.asset_id, "photo-2"],
      checkpoint: { ...cull.run!.checkpoint, pass: [row.asset_id, "photo-2"] },
    };
  });
  await screen.findByRole("button", { name: "Show Photo 2.jpg" });
  await fireEvent.click(await readyButton("Pick (P)"));
  await screen.findByText("Photo changed");
  expect(edit).toHaveBeenCalledOnce();
  expect(edit.mock.calls[0]![0]).toMatchObject({
    targets: [{ file_id: row.display_file_id, content_version_id: row.content_version_id }],
    patch: { flag: "pick" },
  });
  const shell = container.querySelector(".photo-cull")!;
  await fireEvent.keyDown(shell, { key: "ArrowRight" });
  await fireEvent.keyDown(shell, { key: "p" });
  expect(cull.currentID).toBe(row.asset_id);
  expect(screen.getByRole("alert").textContent?.trim()).toBe("Photo changed");
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

it.each(["before edit", "during preparation", "before Undo"])(
  "refuses a stale tab decision %s",
  async (timing) => {
    vi.stubGlobal("indexedDB", new IDBFactory());
    vi.stubGlobal("IDBKeyRange", IDBKeyRange);
    const store = photoCullStorage("synthetic-vault");
    const { cull, photos } = await reviewPhoto(photo(1), true, store);
    await store.replace(cull.run!);
    const other = new PhotoCull("session", photoCullStorage("synthetic-vault"));
    await other.restore();
    const edit = vi.spyOn(api, "editPhotoAuthored");
    const undo = vi.spyOn(api, "undoPhotoAuthored");
    const preparation = photoPreparation([preparedPhoto(1)]);
    let finish!: (value: typeof preparation) => void;
    if (timing === "during preparation") {
      vi.mocked(api.preparePhotoAuthored).mockReturnValueOnce(
        new Promise((resolve) => (finish = resolve)),
      );
      await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
      await waitFor(() => expect(api.preparePhotoAuthored).toHaveBeenCalledOnce());
    }
    other.move(1);
    await other.flush();
    if (timing === "during preparation") await act(() => finish(preparation));
    else if (timing === "before Undo") {
      await act(() => {
        photos.cullEditor!.receiptID = "decision";
        photos.cullEditor!.undoState.summary = "Photo picked";
      });
      await fireEvent.click(await readyButton(/^Undo$/));
    } else await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
    await screen.findByText(
      "Progress could not be saved: Cull progress changed in another tab. Close and resume it.",
    );
    expect(timing === "before Undo" ? undo : edit).not.toHaveBeenCalled();
  },
);

it("keeps a failed decision on screen and guards exit while its draft remains", async () => {
  vi.spyOn(api, "editPhotoAuthored").mockRejectedValue(new Error("Decision failed"));
  const { cull, onclose } = await reviewPhoto();
  await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
  await screen.findByText("Decision failed");
  await fireEvent.keyDown(document.querySelector(".photo-cull")!, { key: "Escape" });
  expect(screen.getByRole("alert").textContent).toContain(
    "Apply or clear your metadata edits before changing photos.",
  );
  expect(cull.currentID).toBe("photo-1");
  await fireEvent.click(screen.getByRole("button", { name: "Exit cull" }));
  expect(onclose).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "Discard edit" }));
  await fireEvent.click(screen.getByRole("button", { name: "Exit cull" }));
  expect(onclose).toHaveBeenCalledOnce();
});

it("fetches only rail photos that are neither displayed nor already held", async () => {
  const { cull, photos } = await reviewPhoto();
  const rows = Array.from({ length: 12 }, (_, index) => photo(index + 1));
  const lookup = vi
    .mocked(photos.lookupMany)
    .mockImplementation(async (ids) => rows.filter((row) => ids.includes(row.asset_id)));
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: rows.map((row) => row.asset_id),
      checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
    };
  });
  await screen.findByRole("button", { name: "Show Photo 5.jpg" });
  expect(screen.getByRole("button", { name: "Show Photo 1.jpg" })).toBeTruthy();
  expect(photos.lookup).toHaveBeenCalledTimes(2);
  expect(lookup.mock.calls.every(([ids]) => !ids.includes("photo-1"))).toBe(true);
  lookup.mockClear();
  vi.mocked(photos.lookup).mockClear();
  await fireEvent.click(screen.getByRole("button", { name: /^Next$/ }));
  await screen.findByRole("button", { name: "Show Photo 6.jpg" });
  expect(lookup).toHaveBeenCalledExactlyOnceWith(["photo-6"], expect.any(AbortSignal));
  expect(screen.getByRole("button", { name: "Show Photo 1.jpg" })).toBeTruthy();
  expect(photos.lookup).not.toHaveBeenCalled();
});

it("reuses the held row after a decision on a RAW/JPEG pair", async () => {
  const jpeg = preparedPhoto(1);
  const raw = preparedPhoto(2, {
    asset_id: jpeg.asset_id,
    file_id: "raw-1",
    file_name: "Photo 1.ARW",
  });
  vi.mocked(api.preparePhotoAuthored).mockResolvedValue(photoPreparation([raw, jpeg]));
  vi.spyOn(api, "editPhotoAuthored").mockResolvedValue({
    receipt_id: "pick",
    before: [raw, jpeg],
    after: [raw, jpeg].map((target) => ({
      ...target,
      revision: 2,
      values: { ...target.values, flag: "pick" },
    })),
  });
  const { cull, photos } = await reviewPhoto(photo(1), true);
  vi.mocked(photos.lookupMany).mockResolvedValue([photo(2)]);
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: ["photo-1", "photo-2"],
      checkpoint: { ...cull.run!.checkpoint, pass: ["photo-1", "photo-2"] },
    };
  });
  await screen.findByRole("button", { name: "Show Photo 2.jpg" });
  await screen.findByText("Photo 1.jpg");
  const lookups = vi.mocked(photos.lookup).mock.calls.length;
  await fireEvent.click(await readyButton("Pick (P)"));
  await screen.findByText("Photo 2.jpg");
  await fireEvent.click(await readyButton("Previous"));
  await screen.findByText("Photo 1.jpg");
  expect(photos.lookup).toHaveBeenCalledTimes(lookups);
});

it.each([
  ["different flags", { flag: "reject" }, "p", "4", "file-1", ["file-1"]],
  ["different ratings", { rating: 2 }, "4", "p", "file-1", ["file-1"]],
  ["equal pair", {}, "p", "4", "", ["raw-1", "file-1"]],
] as const)(
  "keeps shown-file narrowing on retry and resets it for a fresh decision: %s",
  async (_name, values, key, nextKey, fileID, targets) => {
    const jpeg = preparedPhoto(1);
    const raw = preparedPhoto(2, {
      asset_id: jpeg.asset_id,
      file_id: "raw-1",
      values: { ...jpeg.values, ...values },
    });
    vi.mocked(api.preparePhotoAuthored).mockResolvedValue(photoPreparation([raw, jpeg]));
    const edit = vi
      .spyOn(api, "editPhotoAuthored")
      .mockRejectedValueOnce(new TypeError("Failed to fetch"))
      .mockResolvedValue({ receipt_id: "decision", before: [], after: [] });
    const { photos, container } = await reviewPhoto();
    const shell = container.querySelector(".photo-cull")!;
    await fireEvent.keyDown(shell, { key });
    await screen.findByText("Failed to fetch");
    await fireEvent.click(await readyButton("Retry edit"));
    await readyButton("Pick (P)");
    expect(edit).toHaveBeenCalledTimes(2);
    for (const [request] of edit.mock.calls)
      expect(Object.values(request.targets).map((target) => target.file_id)).toEqual(targets);
    expect(photos.cullEditor!.fileID).toBe(fileID);
    if (fileID)
      await waitFor(() =>
        expect(screen.getByRole("alert").textContent).toContain("changed only the shown file"),
      );
    await fireEvent.keyDown(shell, { key: nextKey });
    await waitFor(() => expect(edit).toHaveBeenCalledTimes(3));
    await readyButton("Pick (P)");
    expect(Object.values(edit.mock.calls[2]![0].targets).map((target) => target.file_id)).toEqual([
      "raw-1",
      "file-1",
    ]);
    expect(photos.cullEditor!.fileID).toBe("");
  },
);

it("checks saved progress before Undo when the saved run is unchanged", async () => {
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("IDBKeyRange", IDBKeyRange);
  const store = photoCullStorage("synthetic-vault");
  const { cull, photos } = await reviewPhoto(photo(1), false, store);
  await store.replace(cull.run!);
  const other = new PhotoCull("session", photoCullStorage("synthetic-vault"));
  await other.restore();
  let finishUndo!: () => void;
  const undo = vi.spyOn(api, "undoPhotoAuthored").mockImplementation(async () => {
    await new Promise<void>((resolve) => (finishUndo = resolve));
    return { receipt_id: "undo", before: [], after: [] };
  });
  await act(() => {
    photos.cullEditor!.receiptID = "decision";
    photos.cullEditor!.undoState.summary = "Photo picked";
  });
  let release!: () => void;
  const saved = store.saved;
  vi.spyOn(store, "saved").mockImplementation(async () => {
    await new Promise<void>((resolve) => (release = resolve));
    return saved();
  });
  await fireEvent.click(await readyButton(/^Undo$/));
  await waitFor(() => expect(release).toBeTypeOf("function"));
  expect(screen.getByRole("button", { name: /^Undo$/ }).matches(":disabled")).toBe(true);
  expect(screen.getByRole("button", { name: "Exit cull" }).matches(":disabled")).toBe(true);
  expect(undo).not.toHaveBeenCalled();
  await act(() => release());
  await waitFor(() => expect(undo).toHaveBeenCalledExactlyOnceWith("decision", expect.anything()));
  expect(screen.getByRole("button", { name: /^Undo$/ }).matches(":disabled")).toBe(true);
  await act(() => finishUndo());
  await waitFor(() => expect(screen.queryByRole("button", { name: /^Undo$/ })).toBeNull());
  expect(cull.stale).toBe(false);
});

it.each(["Cancel", "Reset pass"])(
  "%s in the scope modal preserves or replaces the saved pass",
  async (answer) => {
    const { cull } = await reviewPhoto();
    const list = vi.spyOn(api, "lookupPhotoFlags").mockResolvedValue({
      items: [photo(1)],
    });
    await fireEvent.click(await readyButton("Scope: all"));
    await screen.findByRole("dialog", { name: "Change scope and reset this pass's progress?" });
    expect(list).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Exit cull" }).matches(":disabled")).toBe(true);
    await fireEvent.click(screen.getByRole("button", { name: answer }));
    await waitFor(() => expect(cull.busy).toBe(false));
    if (answer === "Cancel") {
      expect(cull.run!.checkpoint.scope).toBe("all");
    } else {
      expect(cull.run!.checkpoint.scope).toBe("unflagged");
      expect(list).toHaveBeenCalledOnce();
    }
  },
);

it("clears selected frames when All changes to Unflagged with the same photos", async () => {
  const rows = [photo(1), photo(2), photo(3)];
  const ids = rows.map((row) => row.asset_id);
  const { cull, photos } = await reviewPhoto(rows[0]!);
  vi.mocked(photos.lookup).mockImplementation(async (id) =>
    rows.find((row) => row.asset_id === id)!,
  );
  vi.mocked(photos.lookupMany).mockResolvedValue(rows);
  vi.spyOn(api, "lookupPhotoFlags").mockResolvedValue({ items: rows });
  vi.spyOn(api, "groupPhotoStacks").mockResolvedValue({
    groups: [ids],
    unavailable_ids: [],
    burst_gap_seconds: 2,
  });
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids,
      groups: [ids.slice(0, 2)],
      burstGapSeconds: 2,
      checkpoint: { ...cull.run!.checkpoint, scope: "all", pass: ids },
    };
  });
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Photo 2.jpg" }));
  expect(screen.getByRole("checkbox", { name: "Select Photo 2.jpg" })).toHaveProperty(
    "checked",
    true,
  );
  await readyButton("Keep current (Return)");
  await fireEvent.click(await readyButton("Scope: all"));
  await screen.findByRole("dialog", { name: "Change scope and reset this pass's progress?" });
  await fireEvent.click(screen.getByRole("button", { name: "Reset pass" }));
  await readyButton("Scope: unflagged");
  expect(cull.run!.checkpoint.pass).toEqual(ids);
  expect(screen.getByRole("checkbox", { name: "Select Photo 2.jpg" })).toHaveProperty(
    "checked",
    false,
  );
});

it.each(["missing", "different version"])(
  "loads the current photo from a %s neighbor",
  async (kind) => {
    const rows = [photo(1), photo(2)];
    const { cull, photos } = await reviewPhoto(rows[0]);
    vi.mocked(photos.lookupMany).mockResolvedValue(kind === "missing" ? [] : [rows[1]!]);
    vi.mocked(photos.lookup).mockImplementation(async (id) =>
      id === "photo-1" ? rows[0]! : { ...rows[1]!, name: "Looked up.jpg" },
    );
    await act(() => {
      cull.run = {
        ...cull.run!,
        ids: rows.map((row) => row.asset_id),
        checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
      };
    });
    await readyButton("Next");
    if (kind !== "missing") await screen.findByRole("button", { name: "Show Photo 2.jpg" });
    if (kind === "different version") {
      await act(() => {
        photos.cullEditor!.targets = [
          { ...preparedPhoto(2), content_version_id: "expected-version" },
        ];
      });
    }
    vi.mocked(photos.lookup).mockClear();
    await fireEvent.click(await readyButton("Next"));
    await screen.findByText("Looked up.jpg");
    expect(photos.lookup).toHaveBeenCalledExactlyOnceWith("photo-2", expect.any(AbortSignal));
  },
);

it.each(["rating only", "flag change", "unavailable mark and auto-advance toggle"])(
  "reads completed-pass flags again only after a flag-changing receipt: %s",
  async (kind) => {
    const row = photo(1);
    if (kind === "flag change") row.flag = "pick";
    const flags = vi.spyOn(api, "lookupPhotoFlags").mockImplementation(async () => ({
      items: [{ ...row }],
    }));
    const before = preparedPhoto(1);
    const undo = vi.spyOn(api, "undoPhotoAuthored").mockImplementation(async () => {
      row.flag = "";
      return {
        receipt_id: "undo",
        before: [
          {
            ...before,
            values: { ...before.values, flag: kind === "flag change" ? "pick" : "" },
          },
        ],
        after: [
          {
            ...before,
            revision: 2,
            values: { ...before.values, rating: kind === "rating only" ? 4 : before.values.rating },
          },
        ],
      };
    });
    const { cull, photos } = await reviewPhoto(row);
    const remainder = () => screen.queryByRole("button", { name: "Review unflagged remainder" });
    if (kind.startsWith("unavailable")) {
      await act(() => {
        cull.run = { ...cull.run!, ids: [row.asset_id, photo(2).asset_id] };
      });
    }
    await fireEvent.click(await readyButton("Next"));
    await screen.findByRole("heading", { name: "Pass complete" });
    await waitFor(() => expect(cull.busy).toBe(false));
    expect(flags).toHaveBeenCalledOnce();
    if (kind.startsWith("unavailable")) {
      await screen.findByText(/1 unavailable/);
      expect(cull.run!.checkpoint.unavailable).toEqual([photo(2).asset_id]);
      await fireEvent.click(screen.getByRole("button", { name: "Auto-advance: off" }));
      await waitFor(() => expect(cull.busy).toBe(false));
      await act(() => cull.unavailable(row.asset_id));
      await waitFor(() => expect(cull.busy).toBe(false));
      expect(flags).toHaveBeenCalledOnce();
      return;
    }
    expect(!!remainder()).toBe(kind !== "flag change");
    await act(() => {
      photos.cullEditor!.receiptID = "previous-edit";
      photos.cullEditor!.undoState.summary = "Previous edit";
    });
    await fireEvent.click(await readyButton(/^Undo$/));
    await screen.findByText("Edit undone");
    expect(undo).toHaveBeenCalledOnce();
    await screen.findByRole("button", { name: "Review unflagged remainder" });
    expect(flags).toHaveBeenCalledTimes(kind === "flag change" ? 2 : 1);
  },
);

it("sends Keep all, selected, and representative through one receipt path and undoes", async () => {
  const rows = [photo(1), photo(2), photo(3)];
  const stack = rows.slice(0, 2).map((row) => row.asset_id);
  const before = preparedPhoto(1);
  const picked = {
    ...before,
    revision: 2,
    values: { ...before.values, flag: "pick" as const },
  };
  const decide = vi.spyOn(api, "decidePhotoStack").mockResolvedValue({
    receipt_id: "stack-decision",
    before: [before],
    after: [picked],
  });
  const undo = vi.spyOn(api, "undoPhotoAuthored").mockResolvedValue({
    receipt_id: "undo-stack",
    before: [picked],
    after: [{ ...before, revision: 3 }],
  });
  const { cull, photos } = await reviewPhoto();
  vi.spyOn(photos, "metadataChanged").mockReturnValue(new Map());
  vi.mocked(photos.lookup).mockImplementation(async (id) =>
    rows.find((row) => row.asset_id === id)!,
  );
  vi.mocked(photos.lookupMany).mockImplementation(async (ids) =>
    rows.filter((row) => ids.includes(row.asset_id)),
  );
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: rows.map((row) => row.asset_id),
      groups: [stack],
      burstGapSeconds: 2,
      checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
    };
  });
  await readyButton("Keep current (Return)");
  expect(screen.getAllByLabelText("Stack boundary")).toHaveLength(2);
  await fireEvent.click(await readyButton("Keep all"));
  await waitFor(() => expect(decide).toHaveBeenCalledTimes(1));
  expect(decide.mock.calls[0]![0]).toMatchObject({
    asset_ids: stack,
    current_id: "photo-1",
    decision: "all",
    burst_gap_seconds: 2,
  });
  await fireEvent.click(await readyButton(/^Undo$/));
  await waitFor(() => expect(undo).toHaveBeenCalledWith("stack-decision", expect.anything()));
  await fireEvent.click(await readyButton("Choose representative"));
  await waitFor(() => expect(decide).toHaveBeenCalledTimes(2));
  expect(decide.mock.calls[1]![0].decision).toBe("representative");
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select Photo 2.jpg" }));
  await fireEvent.click(await readyButton("Keep selected"));
  await waitFor(() => expect(decide).toHaveBeenCalledTimes(3));
  expect(decide.mock.calls[2]![0]).toMatchObject({
    decision: "selected",
    selected_ids: ["photo-2"],
  });
  await screen.findByText("Selected frames kept");
});

it("keeps a frame the pass excludes out of the stack that Keep all decides", async () => {
  const rows = [photo(1), photo(2), photo(3)];
  const pass = rows.slice(1).map((row) => row.asset_id);
  const picked = preparedPhoto(2);
  const decide = vi
    .spyOn(api, "decidePhotoStack")
    .mockResolvedValue({ receipt_id: "all", before: [picked], after: [picked] });
  const group = vi.mocked(api.groupPhotoStacks);
  group.mockImplementation(async (request) => ({
    groups: [Object.values(request.asset_ids)],
    unavailable_ids: [],
    burst_gap_seconds: 2,
  }));
  const { cull, photos } = await reviewPhoto(rows[1]);
  vi.mocked(photos.lookup).mockImplementation(async (id) =>
    rows.find((row) => row.asset_id === id)!,
  );
  vi.mocked(photos.lookupMany).mockImplementation(async (ids) =>
    rows.filter((row) => ids.includes(row.asset_id)),
  );
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: rows.map((row) => row.asset_id),
      checkpoint: { ...cull.run!.checkpoint, pass },
    };
  });
  await act(() => cull.regroup());
  expect(group.mock.lastCall![0].asset_ids).toEqual(pass);
  await fireEvent.click(await readyButton("Keep all"));
  await waitFor(() => expect(decide).toHaveBeenCalledOnce());
  expect(decide.mock.calls[0]![0]).toMatchObject({ asset_ids: pass, decision: "all" });
});

it("keeps individual decisions available while camera details update", async () => {
  const { cull } = await reviewPhoto();
  await act(() => {
    cull.run = { ...cull.run!, groups: [], burstGapSeconds: 2, groupingIncomplete: true };
  });
  await screen.findByText(/Some camera details are missing or outdated/);
  await readyButton("Pick (P)");
  expect(screen.queryByRole("button", { name: "Keep all" })).toBeNull();
  const settings = vi.mocked(api.getPhotoSettings);
  settings.mockClear();
  await fireEvent(window, new Event("focus"));
  await waitFor(() => expect(settings).toHaveBeenCalledOnce());
  const group = vi.mocked(api.groupPhotoStacks);
  group.mockResolvedValueOnce({ groups: [], unavailable_ids: [], burst_gap_seconds: 2 });
  await fireEvent.click(await readyButton("Retry grouping"));
  await waitFor(() => expect(cull.run?.groupingIncomplete).toBeFalsy());
  expect(screen.queryByText(/Some camera details are missing or outdated/)).toBeNull();
});

it("Return visits interleaved stacks and singletons once", async () => {
  const rows = [photo(1), photo(2), photo(3), photo(4), photo(5)];
  const states = new Map(rows.map((row, index) => [row.asset_id, preparedPhoto(index + 1)]));
  vi.mocked(api.preparePhotoAuthored).mockImplementation(async (scope) =>
    photoPreparation((scope.asset_ids ?? []).map((id) => states.get(id)!)),
  );
  const decide = vi.spyOn(api, "decidePhotoStack").mockImplementation(async (request) => {
    const before = Object.values(request.asset_ids).map((id) => states.get(id)!);
    const after = before.map((target) => ({
      ...target,
      revision: target.revision + 1,
      values: {
        ...target.values,
        flag: target.asset_id === request.current_id ? ("pick" as const) : ("reject" as const),
      },
    }));
    for (const target of after) states.set(target.asset_id, target);
    return { receipt_id: "stack-decision", before, after };
  });
  const edit = vi.spyOn(api, "editPhotoAuthored").mockImplementation(async (request) => {
    const before = Object.values(request.targets).map((target) => states.get(target.asset_id)!);
    const after = before.map((target) => ({
      ...target,
      revision: target.revision + 1,
      values: { ...target.values, flag: request.patch.flag ?? "" },
    }));
    for (const target of after) states.set(target.asset_id, target);
    return { receipt_id: "singleton-decision", before, after };
  });
  const { cull, photos, container } = await reviewPhoto(rows[0], true);
  vi.spyOn(photos, "metadataChanged").mockReturnValue(new Map());
  vi.mocked(photos.lookup).mockImplementation(async (id) => {
    const row = rows.find((row) => row.asset_id === id)!;
    return { ...row, flag: states.get(id)!.values.flag };
  });
  vi.mocked(photos.lookupMany).mockImplementation(async (ids) =>
    rows.filter((row) => ids.includes(row.asset_id)),
  );
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: rows.map((row) => row.asset_id),
      groups: [
        ["photo-1", "photo-3"],
        ["photo-2", "photo-5"],
      ],
      burstGapSeconds: 2,
      checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
    };
  });
  const shell = container.querySelector(".photo-cull")!;
  for (const [current, next] of [
    ["photo-1", "photo-2"],
    ["photo-2", "photo-4"],
    ["photo-4", undefined],
  ]) {
    expect(cull.currentID).toBe(current);
    await readyButton(current === "photo-4" ? "Pick (P)" : "Keep current (Return)");
    await fireEvent.keyDown(shell, { key: "Enter" });
    await waitFor(() => expect(cull.currentID).toBe(next));
  }
  expect(cull.complete).toBe(true);
  expect(decide.mock.calls.map(([request]) => request.current_id)).toEqual(["photo-1", "photo-2"]);
  expect(edit).toHaveBeenCalledOnce();
  expect(edit.mock.calls[0]![0]).toMatchObject({
    targets: [{ asset_id: "photo-4" }],
    patch: { flag: "pick" },
  });
});

async function reviewThree() {
  const rows = [photo(1), photo(2), photo(3)];
  const review = await reviewPhoto(rows[0]);
  const { cull, photos } = review;
  vi.mocked(photos.lookup).mockImplementation(async (id) =>
    rows.find((row) => row.asset_id === id)!,
  );
  vi.mocked(photos.lookupMany).mockImplementation(async (ids) =>
    rows.filter((row) => ids.includes(row.asset_id)),
  );
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: rows.map((row) => row.asset_id),
      checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
    };
  });
  return { ...review, rows };
}

it("swaps a changed photo in place and marks one that left the view unavailable", async () => {
  const { cull, photos, rows } = await reviewThree();
  vi.mocked(photos.lookupMany).mockResolvedValueOnce([{ ...rows[0], name: "Renamed.jpg" }]);
  await act(() => photos.changed({}));
  await screen.findByText("Renamed.jpg");
  expect(screen.queryByText("Loading photo…")).toBeNull();
  vi.mocked(photos.lookupMany).mockResolvedValueOnce([]);
  await act(() => photos.changed({}));
  await screen.findByText("This photo is no longer in this view.");
  expect(screen.queryByText("Renamed.jpg")).toBeNull();
  expect(cull.run!.checkpoint.unavailable).toEqual(["photo-1"]);
  // Another tab restores the photo.
  vi.mocked(photos.lookupMany).mockResolvedValueOnce([{ ...rows[0], name: "Restored.jpg" }]);
  await act(() => photos.changed({}));
  await screen.findByText("Restored.jpg");
  expect(screen.queryByText("This photo is no longer in this view.")).toBeNull();
});

it("live metadata completion rebuilds the active Cull groups", async () => {
  const { cull, photos } = await reviewThree();
  await act(() => {
    cull.run = { ...cull.run!, groups: [], burstGapSeconds: 2, groupingIncomplete: true };
  });
  await screen.findByText(/Some camera details are missing or outdated/);
  vi.mocked(api.groupPhotoStacks).mockResolvedValue({
    groups: [["photo-1", "photo-2"]],
    unavailable_ids: [],
    burst_gap_seconds: 2,
  });
  await act(() => photos.changed({}));
  expect(screen.queryByText(/Some camera details are missing or outdated/)).toBeNull();
  expect(screen.getByRole("button", { name: "Keep all" })).toBeTruthy();
});

it("keeps Cull usable while a live regroup is pending", async () => {
  const { cull, photos, container } = await reviewThree();
  await act(() => {
    cull.run = { ...cull.run!, groups: [["photo-1", "photo-2"]], burstGapSeconds: 2 };
  });
  await readyButton("Keep all");
  let finish!: (value: Awaited<ReturnType<typeof api.groupPhotoStacks>>) => void;
  vi.mocked(api.groupPhotoStacks).mockImplementationOnce(
    () => new Promise((resolve) => (finish = resolve)),
  );
  const changing = photos.changed({});
  await waitFor(() => expect(finish).toBeDefined());
  expect(cull.stackLoading).toBe(false);
  expect(screen.queryByText("Grouping bursts...")).toBeNull();
  await readyButton("Next");
  await readyButton("Exit cull");
  await fireEvent.keyDown(container.querySelector(".photo-cull")!, { key: "ArrowRight" });
  await waitFor(() => expect(cull.currentID).toBe("photo-2"));
  await act(async () => {
    finish({ groups: [], unavailable_ids: [], burst_gap_seconds: 2 });
  });
  await changing;
  expect(cull.currentID).toBe("photo-2");
  expect(screen.queryByRole("button", { name: "Keep all" })).toBeNull();
});

it("keeps the previous groups when a live regroup fails and tries again on the retry", async () => {
  const { cull, photos } = await reviewThree();
  await act(() => {
    cull.run = { ...cull.run!, groups: [["photo-1", "photo-2"]], burstGapSeconds: 2 };
  });
  await readyButton("Keep all");
  const group = vi.mocked(api.groupPhotoStacks);
  group.mockRejectedValueOnce(new APIError("Groups down", 503, "unavailable"));
  await expect(act(() => photos.changed({}))).rejects.toThrow("Groups down");
  expect(cull.stackError).toBe("");
  expect(cull.stackLoading).toBe(false);
  expect(screen.queryByText("Groups down")).toBeNull();
  expect(screen.getByRole("button", { name: "Keep all" })).toBeTruthy();
  group.mockResolvedValueOnce({ groups: [], unavailable_ids: [], burst_gap_seconds: 2 });
  await act(() => photos.changed({}));
  expect(screen.queryByRole("button", { name: "Keep all" })).toBeNull();
});

it("restores Cull decisions after a live refresh recovers a grouping failure", async () => {
  const { cull, photos } = await reviewThree();
  await act(() => {
    cull.run = { ...cull.run!, groups: [["photo-1", "photo-2"]], burstGapSeconds: 2 };
  });
  await readyButton("Keep all");
  const group = vi.mocked(api.groupPhotoStacks);
  group.mockRejectedValueOnce(new APIError("Groups temporarily unavailable", 503, "unavailable"));
  await act(() => cull.regroup());
  expect(screen.getByRole("button", { name: "Keep all" }).matches(":disabled")).toBe(true);
  group.mockResolvedValueOnce({
    groups: [["photo-1", "photo-2"]],
    unavailable_ids: [],
    burst_gap_seconds: 2,
  });
  await act(() => photos.changed({}));
  expect(cull.stackError).toBe("");
  expect(screen.getByRole("button", { name: "Keep all" }).matches(":disabled")).toBe(false);
});

async function reviewTwoStacks() {
  const rows = [photo(1), photo(2), photo(3), photo(4)];
  const review = await reviewPhoto(rows[0]);
  const { cull, photos } = review;
  vi.mocked(photos.lookup).mockImplementation(async (id) =>
    rows.find((row) => row.asset_id === id)!,
  );
  vi.mocked(photos.lookupMany).mockImplementation(async (ids) =>
    rows.filter((row) => ids.includes(row.asset_id)),
  );
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: rows.map((row) => row.asset_id),
      groups: [
        ["photo-1", "photo-2"],
        ["photo-3", "photo-4"],
      ],
      burstGapSeconds: 2,
      checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
    };
  });
  await fireEvent.click(await screen.findByRole("checkbox", { name: "Select Photo 2.jpg" }));
  await readyButton("Keep selected");
  return review;
}

it.each([
  { change: "leaves", groups: [["photo-1", "photo-2"]], kept: true },
  { change: "changes", groups: [["photo-1", "photo-2", "photo-3"]], kept: false },
])(
  "a live regroup that $change a stack keeps its checked frames only if kept is $kept",
  async ({ groups, kept }) => {
    const { photos } = await reviewTwoStacks();
    vi.mocked(api.groupPhotoStacks).mockResolvedValue({
      groups,
      unavailable_ids: [],
      burst_gap_seconds: 2,
    });
    await act(() => photos.changed({}));
    const checkbox = screen.getByRole("checkbox", {
      name: "Select Photo 2.jpg",
    }) as HTMLInputElement;
    expect(checkbox.checked).toBe(kept);
    expect(
      (screen.getByRole("button", { name: "Keep selected" }) as HTMLButtonElement).disabled,
    ).toBe(!kept);
  },
);

it.each([
  ["a live change", 2, false],
  ["a live change while the read fails", 3, true],
])("rereads the unflagged remainder of a completed pass after %s", async (_kind, reads, fails) => {
  const flags = vi
    .spyOn(api, "lookupPhotoFlags")
    .mockImplementation(async () => ({ items: [photo(1)] }));
  const { cull, photos } = await reviewPhoto();
  await fireEvent.click(await readyButton("Next"));
  await screen.findByRole("heading", { name: "Pass complete" });
  await waitFor(() => expect(cull.busy).toBe(false));
  expect(flags).toHaveBeenCalledOnce();
  if (fails) {
    // The failed read fails the pass, so the watcher runs it again.
    flags.mockRejectedValueOnce(new APIError("Flags down", 503, "unavailable"));
    await expect(act(() => photos.changed({}))).rejects.toThrow("Flags down");
    await act(() => photos.changed({}));
    await readyButton("Review unflagged remainder");
  } else await act(() => photos.changed({}));
  await waitFor(() => expect(cull.busy).toBe(false));
  expect(flags).toHaveBeenCalledTimes(reads);
});

// An older read must not replace what a newer one showed, whichever answers last.
async function reviewWithPendingReads() {
  const rows = [photo(1), photo(2), photo(3)];
  const review = await reviewPhoto(rows[0]);
  const { cull, photos } = review;
  let foreground!: (row: PhotoBrowseRow) => void;
  let rail!: (rows: PhotoBrowseRow[]) => void;
  vi.mocked(photos.lookup).mockImplementationOnce(
    () => new Promise((resolve) => (foreground = resolve)),
  );
  vi.mocked(photos.lookupMany).mockImplementationOnce(
    () => new Promise((resolve) => (rail = resolve)),
  );
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: rows.map((row) => row.asset_id),
      checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
    };
  });
  await waitFor(() => expect(foreground).toBeDefined());
  await waitFor(() => expect(rail).toBeDefined());
  return { ...review, rows, foreground, rail };
}

it.each([
  {
    read: "foreground lookup",
    index: 0,
    query: (name: string) => screen.queryByText(name),
    older: (rows: PhotoBrowseRow[]) => ({ ...rows[0]!, name: "Older.jpg" }),
  },
  {
    read: "rail lookup",
    index: 1,
    query: (name: string) => screen.queryByLabelText(new RegExp(name.replace(".", "\\."))),
    older: (rows: PhotoBrowseRow[]) => [{ ...rows[1]!, name: "Older.jpg" }, rows[2]!],
  },
])("keeps a live row when an older $read answers after it", async ({ index, query, older }) => {
  const { photos, rows, foreground, rail } = await reviewWithPendingReads();
  vi.mocked(photos.lookupMany).mockResolvedValueOnce([{ ...rows[index]!, name: "Newer.jpg" }]);
  await act(() => photos.changed({}));
  await waitFor(() => expect(query("Newer.jpg")).toBeTruthy());
  (index === 0 ? foreground : rail)(older(rows) as never);
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(query("Older.jpg")).toBeNull();
  expect(query("Newer.jpg")).toBeTruthy();
});

it("shows a cached neighbor's live row after navigation during its read", async () => {
  const { photos, rows } = await reviewThree();
  await screen.findByLabelText(/Photo 2.jpg/);
  let finish!: (rows: PhotoBrowseRow[]) => void;
  vi.mocked(photos.lookupMany).mockImplementationOnce(
    () => new Promise((resolve) => (finish = resolve)),
  );
  const reading = photos.changed({});
  await waitFor(() => expect(finish).toBeDefined());
  await fireEvent.click(await readyButton("Next"));
  await screen.findByText("Photo 2.jpg");
  await act(async () => {
    finish([rows[0]!, { ...rows[1]!, name: "Updated neighbor.jpg" }]);
    await reading;
  });
  await screen.findByText("Updated neighbor.jpg");
  expect(screen.queryByText("Photo 2.jpg")).toBeNull();
  photos.dispose();
});

it("commits the next pass when a live event arrives during its flag lookup", async () => {
  const flags = vi.spyOn(api, "lookupPhotoFlags").mockResolvedValue({ items: [photo(1)] });
  const { cull, photos } = await reviewPhoto();
  await fireEvent.click(await readyButton("Next"));
  await screen.findByRole("heading", { name: "Pass complete" });
  await readyButton("Review unflagged remainder");
  let finish!: (value: Awaited<ReturnType<typeof api.lookupPhotoFlags>>) => void;
  let signal!: AbortSignal;
  flags.mockImplementationOnce((_request, options) => {
    signal = options!.signal!;
    return new Promise((resolve) => (finish = resolve));
  });
  await fireEvent.click(await readyButton("Review unflagged remainder"));
  await waitFor(() => expect(finish).toBeDefined());
  const changing = photos.changed({});
  await act(async () => {
    finish({ items: [photo(1)] });
  });
  await changing;
  await screen.findByText("Photo 1.jpg");
  expect(signal.aborted).toBe(false);
  expect(cull.run!.checkpoint.scope).toBe("unflagged");
  expect(cull.run!.checkpoint.position).toBe(0);
  photos.dispose();
});

it.each([1, 7])("keeps all with fresh revisions after stack member %i changes", async (changed) => {
  const rows = Array.from({ length: 7 }, (_, index) => photo(index + 1));
  const stack = rows.map((row) => row.asset_id);
  const targets = rows.map((_, index) => preparedPhoto(index + 1));
  vi.mocked(api.preparePhotoAuthored).mockImplementation(async (scope) =>
    photoPreparation(targets.filter((target) => scope.asset_ids?.includes(target.asset_id))),
  );
  const decide = vi.spyOn(api, "decidePhotoStack").mockImplementation(async () => ({
    receipt_id: "kept-all",
    before: targets.map(({ file_id, node_id, revision, values }) => ({
      file_id,
      node_id,
      revision,
      values,
    })),
    after: targets.map(({ file_id, node_id, revision, values }) => ({
      file_id,
      node_id,
      revision: revision + 1,
      values: { ...values, flag: "pick", confirmed_fields: (values.confirmed_fields ?? 0) | 2 },
    })),
  }));
  const { cull, photos } = await reviewPhoto(rows[0]);
  vi.mocked(photos.lookup).mockImplementation(async (id) =>
    rows.find((row) => row.asset_id === id)!,
  );
  vi.mocked(photos.lookupMany).mockImplementation(async (ids) =>
    rows.filter((row) => ids.includes(row.asset_id)),
  );
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: stack,
      groups: [stack],
      burstGapSeconds: 2,
      checkpoint: { ...cull.run!.checkpoint, pass: stack },
    };
  });
  vi.mocked(api.groupPhotoStacks).mockResolvedValue({
    groups: [stack],
    unavailable_ids: [],
    burst_gap_seconds: 2,
  });
  await readyButton("Keep all");
  expect(screen.queryByRole("button", { name: "Show Photo 7.jpg" })).toBeNull();
  rows[changed - 1] = { ...rows[changed - 1]!, revision: 2, rating: 4 };
  targets[changed - 1] = preparedPhoto(changed, {
    asset_revision: 2,
    revision: 2,
    values: { ...preparedPhoto(changed).values, rating: 4 },
  });
  await act(() => photos.changed({}));
  if (changed === 1) await screen.findByText("Unflagged · 4 stars");
  await fireEvent.click(await readyButton("Keep all"));
  await waitFor(() => expect(decide).toHaveBeenCalledOnce());
  expect(decide.mock.calls[0]![0]).toMatchObject({
    decision: "all",
    asset_ids: stack,
    targets: expect.arrayContaining([
      expect.objectContaining({ asset_id: `photo-${changed}`, asset_revision: 2, revision: 2 }),
    ]),
  });
  await screen.findByRole("button", { name: /^Undo$/ });
  photos.dispose();
});

it("leaves saved progress alone when a live lookup finishes after Exit", async () => {
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("IDBKeyRange", IDBKeyRange);
  const store = photoCullStorage("synthetic-vault");
  const { cull, photos, onclose, unmount } = await reviewPhoto(photo(1), false, store);
  await store.replace(cull.run!);
  let finish!: (rows: PhotoBrowseRow[]) => void;
  let signal!: AbortSignal;
  vi.mocked(photos.lookupMany).mockImplementationOnce((_ids, active) => {
    signal = active!;
    return new Promise((resolve) => (finish = resolve));
  });
  const reading = photos.changed({});
  await waitFor(() => expect(finish).toBeDefined());
  await fireEvent.click(await readyButton("Exit cull"));
  expect(onclose).toHaveBeenCalledOnce();
  await cull.flush();
  unmount();
  const progress = vi.spyOn(store, "checkpoint");
  await act(async () => {
    finish([]);
    await reading;
    await cull.flush();
  });
  expect(progress).not.toHaveBeenCalled();
  expect(cull.run!.checkpoint.unavailable).toEqual([]);
  expect(signal.aborted).toBe(true);
  photos.dispose();
});

it.each(["updated", "removed"])(
  "drops an older conflict-recovery lookup after a live photo is %s",
  async (outcome) => {
    vi.spyOn(api, "editPhotoAuthored").mockRejectedValue(
      new APIError("Photo changed", 409, "photo_display_changed"),
    );
    const { photos, container } = await reviewPhoto();
    await fireEvent.keyDown(container.querySelector(".photo-cull")!, { key: "5" });
    await readyButton("Reload photo");
    let finish!: (row: PhotoBrowseRow) => void;
    vi.mocked(photos.lookup).mockImplementationOnce(
      () => new Promise((resolve) => (finish = resolve)),
    );
    await fireEvent.click(screen.getByRole("button", { name: "Reload photo" }));
    await waitFor(() => expect(finish).toBeDefined());
    vi.mocked(photos.lookupMany).mockResolvedValue(
      outcome === "updated" ? [{ ...photo(1), name: "Latest.jpg" }] : [],
    );
    await act(() => photos.changed({}));
    await screen.findByText(
      outcome === "updated" ? "Latest.jpg" : "This photo is no longer in this view.",
    );
    await act(() => finish(photo(1)));
    expect(screen.queryByText("Photo 1.jpg")).toBeNull();
    expect(
      screen.getByText(
        outcome === "updated" ? "Latest.jpg" : "This photo is no longer in this view.",
      ),
    ).toBeTruthy();
    photos.dispose();
  },
);
