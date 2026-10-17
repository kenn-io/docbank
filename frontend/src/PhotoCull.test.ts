import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { APIError } from "./api-transport.js";
import PhotoCullView from "./PhotoCull.svelte";
import PhotoCullConfirmation from "./PhotoCullConfirmation.svelte";
import { PhotoCull } from "./photoCull.svelte.js";
import { Photos } from "./photos.svelte.js";
import { photo, preparedPhoto, photoPreparation } from "./photo-test-fixtures.js";
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
  vi.spyOn(api, "getPhotoSettings").mockResolvedValue({
    revision: 1,
    preference: null,
    burst_gap_seconds: 2,
  });
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
    { key: "Enter", patch: { flag: "pick" }, announcement: "pick", next: "photo-2" },
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
  vi.mocked(photos.lookup).mockImplementation(
    async (id) => rows.find((row) => row.asset_id === id)!,
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
      "h",
      "j",
      "k",
      "l",
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
    .spyOn(api, "listPhotoAssets")
    .mockRejectedValueOnce(new APIError("Service unavailable", 503, "unavailable"))
    .mockResolvedValue({ items: [photo(1)], total: 1, facets: [], relevance_available: false });
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

it.each([false, true])(
  "retries a failed lookup without leaving the current photo, navigated=%s",
  async (navigated) => {
    const row = photo(navigated ? 2 : 1);
    const cause = new APIError("Temporarily unavailable", 503, "unavailable");
    const { cull, photos, container } = await reviewPhoto(
      navigated ? photo(1) : row,
      true,
      undefined,
      navigated ? {} : { lookupError: cause },
    );
    const edit = vi.spyOn(api, "editPhotoAuthored");
    const lookup = vi.mocked(photos.lookup);
    if (navigated) {
      lookup.mockRejectedValueOnce(cause).mockResolvedValueOnce(row);
      await act(() => {
        cull.run = {
          ...cull.run!,
          ids: ["photo-1", "photo-2"],
          checkpoint: { ...cull.run!.checkpoint, pass: ["photo-1", "photo-2"] },
        };
        cull.move(1);
      });
    }
    const retry = await screen.findByRole("button", { name: /^Retry$/ });
    expect(screen.queryByText("Photo 1.jpg")).toBeNull();
    expect(screen.getByRole("button", { name: "Pick (P)" }).matches(":disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Reject (X)" }).matches(":disabled")).toBe(true);
    await fireEvent.keyDown(container.querySelector(".photo-cull")!, { key: "p" });
    await fireEvent.keyDown(container.querySelector(".photo-cull")!, { key: "x" });
    expect(edit).not.toHaveBeenCalled();
    expect(cull.currentID).toBe(row.asset_id);
    await fireEvent.click(retry);
    await screen.findByText(row.name);
    await readyButton("Pick (P)");
    expect(lookup.mock.calls.map(([id]) => id)).toEqual(
      navigated ? ["photo-1", "photo-2", "photo-2"] : ["photo-1", "photo-1"],
    );
    expect(cull.currentID).toBe(row.asset_id);
    expect(cull.run!.checkpoint.position).toBe(navigated ? 1 : 0);
    expect(screen.queryByRole("button", { name: /^Retry$/ })).toBeNull();
  },
);

async function reviewPhoto(
  row = photo(1),
  autoAdvance = false,
  store?: ReturnType<typeof photoCullStorage>,
  options: { lookupError?: Error } = {},
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
    burstGapSeconds: 2,
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
  const photos = new Photos("session", vi.fn());
  const lookup = vi.spyOn(photos, "lookup").mockResolvedValue(row);
  if (options.lookupError) lookup.mockRejectedValueOnce(options.lookupError);
  vi.spyOn(photos, "lookupMany").mockResolvedValue([row]);
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  const onclose = vi.fn();
  const view = render(PhotoCullView, {
    cull,
    photos,
    cache: {
      get: vi.fn(async () => new Blob(["synthetic-photo"])),
    } as unknown as PhotoPreviewCache,
    onclose,
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
  vi.mocked(photos.lookup).mockImplementation(
    async (id) => rows.find((row) => row.asset_id === id)!,
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
  vi.spyOn(api, "listPhotoAssets").mockResolvedValue({
    items: rows,
    total: 2,
    facets: [],
    relevance_available: false,
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

it.each([
  "before edit",
  "during preparation",
  "before Undo",
  "stack preparation",
  "stack submission",
])("refuses a stale tab decision %s", async (timing) => {
  vi.stubGlobal("indexedDB", new IDBFactory());
  vi.stubGlobal("IDBKeyRange", IDBKeyRange);
  const store = photoCullStorage("synthetic-vault");
  const stack = timing.startsWith("stack");
  const { cull, photos, container } = await reviewPhoto(photo(stack ? 2 : 1), true, store);
  const edit = vi.spyOn(api, "editPhotoAuthored");
  const undo = vi.spyOn(api, "undoPhotoAuthored");
  const decide = vi.spyOn(api, "decidePhotoStack");
  if (stack) {
    const rows = [photo(1), photo(2), photo(3), photo(4)];
    vi.mocked(photos.lookup).mockImplementation(
      async (id) => rows.find((row) => row.asset_id === id)!,
    );
    const run = {
      ...cull.run!,
      ids: rows.map((row) => row.asset_id),
      groups: [["photo-1", "photo-2", "photo-3"]],
      checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id), position: 1 },
    };
    await store.replace(run);
    const saved = vi.spyOn(store, "saved");
    let finish!: (value: Awaited<ReturnType<typeof store.saved>>) => void;
    const pending = new Promise<Awaited<ReturnType<typeof store.saved>>>(
      (resolve) => (finish = resolve),
    );
    if (timing === "stack preparation") saved.mockReturnValueOnce(pending);
    await act(() => {
      cull.run = run;
    });
    if (timing === "stack submission") {
      const keep = await readyButton("Keep all");
      saved
        .mockReturnValueOnce(pending)
        .mockResolvedValueOnce({ ...cull.run!.checkpoint, revision: 99 });
      await fireEvent.click(keep);
    } else await waitFor(() => expect(saved).toHaveBeenCalledOnce());
    for (const key of ["h", "j", "k", "l"])
      await fireEvent.keyDown(container.querySelector(".photo-cull")!, { key });
    expect(cull.currentID).toBe("photo-2");
    if (timing === "stack preparation") expect(api.preparePhotoAuthored).not.toHaveBeenCalled();
    await act(() =>
      finish({
        ...cull.run!.checkpoint,
        revision: timing === "stack preparation" ? 99 : cull.run!.checkpoint.revision,
      }),
    );
  } else {
    await store.replace(cull.run!);
    const other = new PhotoCull("session", photoCullStorage("synthetic-vault"));
    await other.restore();
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
  }
  await screen.findByText(
    "Progress could not be saved: Cull progress changed in another tab. Close and resume it.",
  );
  expect(edit).not.toHaveBeenCalled();
  expect(undo).not.toHaveBeenCalled();
  expect(decide).not.toHaveBeenCalled();
  if (stack) expect(cull.currentID).toBe("photo-2");
  if (timing === "stack preparation") expect(api.preparePhotoAuthored).not.toHaveBeenCalled();
});

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

it("keeps shown-file narrowing on retry and resets it for a fresh decision", async () => {
  const jpeg = preparedPhoto(1);
  const raw = preparedPhoto(2, {
    asset_id: jpeg.asset_id,
    file_id: "raw-1",
    values: { ...jpeg.values, flag: "reject" },
  });
  vi.mocked(api.preparePhotoAuthored).mockResolvedValue(photoPreparation([raw, jpeg]));
  const edit = vi
    .spyOn(api, "editPhotoAuthored")
    .mockRejectedValueOnce(new TypeError("Failed to fetch"))
    .mockResolvedValue({ receipt_id: "decision", before: [], after: [] });
  const { container } = await reviewPhoto();
  await fireEvent.click(screen.getByRole("button", { name: "Pick (P)" }));
  await screen.findByText("Failed to fetch");
  await fireEvent.click(await readyButton("Retry edit"));
  await waitFor(() =>
    expect(container.querySelector(".photo-cull > span[aria-live='polite']")?.textContent).toBe(
      "Photo 1.jpg: pick (shown file only)",
    ),
  );
  expect(edit.mock.calls[1]![0]).toEqual(edit.mock.calls[0]![0]);
  await fireEvent.keyDown(container.querySelector(".photo-cull")!, { key: "4" });
  await waitFor(() => expect(edit).toHaveBeenCalledTimes(3));
  expect(edit.mock.calls[2]![0].patch).toEqual({ rating: 4 });
  await waitFor(() =>
    expect(container.querySelector(".photo-cull > span[aria-live='polite']")?.textContent).toBe(
      "Photo 1.jpg: 4 stars",
    ),
  );
  expect(
    (edit.mock.calls[2]![0].targets as api.PhotoAuthoredEditTarget[]).map(
      (target) => target.file_id,
    ),
  ).toEqual(["raw-1", "file-1"]);
});

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
    const list = vi.spyOn(api, "listPhotoAssets").mockResolvedValue({
      items: [photo(1)],
      total: 1,
      facets: [],
      relevance_available: false,
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
    const flags = vi.spyOn(api, "listPhotoAssets").mockImplementation(async () => ({
      items: [{ ...row }],
      total: 1,
      facets: [],
      relevance_available: false,
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

it("decides the whole saved stack from a later pass", async () => {
  const rows = [photo(1), { ...photo(2), flag: "pick" as const }, photo(3), photo(4)];
  const ids = rows.map((row) => row.asset_id),
    pass = [ids[0]!, ids[2]!, ids[3]!];
  const decide = vi.spyOn(api, "decidePhotoStack").mockResolvedValue({
    receipt_id: "stack",
    before: [preparedPhoto(1), preparedPhoto(3)],
    after: [
      { ...preparedPhoto(1), revision: 2, values: { ...preparedPhoto(1).values, flag: "pick" } },
      { ...preparedPhoto(3), revision: 2, values: { ...preparedPhoto(3).values, flag: "reject" } },
    ],
  });
  const group = vi.spyOn(api, "groupPhotoStacks");
  vi.spyOn(api, "getPhotoSettings").mockResolvedValue({
    revision: 1,
    preference: null,
    burst_gap_seconds: 2,
  });
  const { cull, photos } = await reviewPhoto(rows[0], true);
  vi.mocked(photos.lookup).mockImplementation(
    async (id) => rows.find((row) => row.asset_id === id)!,
  );
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids,

      groups: [ids.slice(0, 3)],
      burstGapSeconds: 2,
      checkpoint: { ...cull.run!.checkpoint, pass, scope: "unflagged" },
    };
  });
  const keep = await readyButton("Keep current (Return)");
  await fireEvent.click(keep);
  await waitFor(() => expect(cull.currentID).toBe(ids[3]));
  expect(decide.mock.calls[0]?.[0]).toMatchObject({
    asset_ids: ids.slice(0, 3),
    current_id: ids[0],
    hidden: false,
  });
  expect(
    Object.values(decide.mock.calls[0]![0].targets)
      .map((target) => target.asset_id)
      .sort(),
  ).toEqual(ids.slice(0, 3));
  expect(group).not.toHaveBeenCalled();
});

it.each(["decision", "representative", "unchanged"])(
  "decides consecutive stacks after a successful %s",
  async (first) => {
    const rows = [photo(1), photo(2), photo(3), photo(4)];
    if (first === "unchanged") {
      rows[0]!.flag = "pick";
      rows[1]!.flag = "pick";
      vi.mocked(api.preparePhotoAuthored).mockImplementation(async (scope) =>
        photoPreparation(
          (scope.asset_ids ?? []).map((id) => {
            const target = preparedPhoto(Number(id.slice(6)));
            return {
              ...target,
              values: { ...target.values, flag: rows.find((row) => row.asset_id === id)!.flag },
            };
          }),
        ),
      );
    }
    const { cull, photos } = await reviewPhoto(rows[0], true);
    vi.mocked(photos.lookup).mockImplementation(
      async (id) => rows.find((row) => row.asset_id === id)!,
    );
    vi.mocked(photos.lookupMany).mockImplementation(async (ids) =>
      rows.filter((row) => ids.includes(row.asset_id)),
    );
    const decide = vi.spyOn(api, "decidePhotoStack").mockImplementation(async (request) => {
      const before = Object.values(request.asset_ids).map((id) =>
        preparedPhoto(Number(id.slice(6))),
      );
      return {
        receipt_id: "stack",
        before,
        after: before.map((target) => ({
          ...target,
          revision: 2,
          values: {
            ...target.values,
            flag:
              request.decision === "all" || target.asset_id === request.current_id
                ? ("pick" as const)
                : ("reject" as const),
          },
        })),
      };
    });
    if (first === "representative")
      decide.mockResolvedValueOnce({
        receipt_id: "representative",
        before: [],
        after: [],
        representatives: [
          { asset_id: "photo-1", before_revision: 1, after_revision: 2, after: 10 },
        ],
      });
    else if (first === "unchanged")
      decide.mockResolvedValueOnce({ receipt_id: "", before: [], after: [] });
    await act(() => {
      cull.run = {
        ...cull.run!,
        ids: rows.map((row) => row.asset_id),

        groups: [
          ["photo-1", "photo-2"],
          ["photo-3", "photo-4"],
        ],
        checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
      };
    });
    if (first === "representative") {
      await screen.findByRole("button", { name: "Show Photo 2.jpg" });
      await fireEvent.click(await readyButton("Choose representative"));
      await waitFor(() => expect(decide).toHaveBeenCalledOnce());
      await readyButton("Keep all");
      await fireEvent.keyDown(document.querySelector(".photo-cull")!, { key: "Enter" });
      await waitFor(() => expect(decide).toHaveBeenCalledTimes(2));
      expect(
        Object.values(decide.mock.calls[1]![0].targets).map((target) => target.asset_revision),
      ).toEqual([2, 1]);
    } else await fireEvent.click(await readyButton("Keep all"));
    await waitFor(() => expect(cull.currentID).toBe("photo-3"));
    await fireEvent.click(await readyButton("Keep all"));
    await screen.findByRole("heading", { name: "Pass complete" });
    expect(decide.mock.calls.at(-1)![0]).toMatchObject({
      asset_ids: ["photo-3", "photo-4"],
      current_id: "photo-3",
      decision: "all",
    });
    expect(
      Object.values(decide.mock.calls.at(-1)![0].targets)
        .map((target) => target.asset_id)
        .sort(),
    ).toEqual(["photo-3", "photo-4"]);
    expect(screen.queryByRole("button", { name: "Reload photo" })).toBeNull();
    expect(document.querySelector(".photo-cull > span[aria-live='polite']")?.textContent).toBe(
      "Stack kept",
    );
  },
);

it.each(["Retry", "next stack"])("prepares after a stack failure through %s", async (action) => {
  const { cull, photos, container } = await reviewPhoto();
  const rows = [photo(1), photo(2), photo(3), photo(4)];
  vi.mocked(photos.lookup).mockImplementation(
    async (id) => rows.find((row) => row.asset_id === id)!,
  );
  vi.mocked(photos.lookupMany).mockImplementation(async (ids) =>
    rows.filter((row) => ids.includes(row.asset_id)),
  );
  vi.mocked(api.preparePhotoAuthored).mockRejectedValueOnce(new Error("Preparation unavailable"));
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: rows.map((row) => row.asset_id),
      groups: [
        ["photo-1", "photo-2"],
        ["photo-3", "photo-4"],
      ],
      checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
    };
  });
  await screen.findByText("Preparation unavailable");
  if (action === "Retry") await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  else await fireEvent.keyDown(container.querySelector(".photo-cull")!, { key: "l" });
  await readyButton("Keep all");
  expect(screen.queryByText("Preparation unavailable")).toBeNull();
});

it("finishes a Cull decision while the affected Library stack refresh is held", async () => {
  const before = preparedPhoto(1);
  vi.spyOn(api, "editPhotoAuthored").mockResolvedValue({
    receipt_id: "picked",
    before: [before],
    after: [{ ...before, revision: 2, values: { ...before.values, flag: "pick" } }],
  });
  const rows = [photo(1), photo(2)];
  const { cull, photos } = await reviewPhoto(rows[0], true);
  vi.mocked(photos.lookup).mockImplementation(
    async (id) => rows.find((row) => row.asset_id === id)!,
  );
  vi.spyOn(photos, "reloadCounts").mockReturnValue(undefined);
  const stack = { asset_ids: ["photo-1", "photo-3"], representative_id: "photo-1" };
  photos.items = [
    { ...photo(1), stack },
    { ...photo(3), stack },
  ];
  photos.started = true;
  let release!: (value: boolean) => void;
  const refresh = vi.spyOn(photos, "refresh").mockReturnValue(
    new Promise((resolve) => {
      release = resolve;
    }),
  );
  await act(() => {
    cull.run = {
      ...cull.run!,
      ids: rows.map((row) => row.asset_id),
      checkpoint: { ...cull.run!.checkpoint, pass: rows.map((row) => row.asset_id) },
    };
  });
  try {
    await fireEvent.click(await readyButton("Pick (P)"));
    await waitFor(() => expect(refresh).toHaveBeenCalledOnce());
    await waitFor(() => expect(cull.currentID).toBe("photo-2"));
    await readyButton("Pick (P)");
  } finally {
    await act(() => release(true));
  }
});
