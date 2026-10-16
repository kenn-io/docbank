import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoStage from "./PhotoStage.svelte";
import { Photos } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import { fittedPhotoView } from "./photoView.js";
import { photo } from "./photo-test-fixtures.js";
import { APIError } from "./api-transport.js";
import { tick } from "svelte";
import type { PhotoPreviewSlot } from "./generated/docbank.js";

afterEach(() => {
  cleanup();
  // jsdom has no HTMLImageElement.decode, so the test defines it and this removes it again.
  delete (HTMLImageElement.prototype as { decode?: unknown }).decode;
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it("retries a failed large preview on the same photo and keeps the fitted image", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  let counter = 0;
  vi.spyOn(URL, "createObjectURL").mockImplementation(() => `blob:synthetic-${++counter}`);
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: vi.fn(async () => {}),
  });
  const item = photo(1);
  item.previews.grid = { state: "ready", generation_id: "grid-1" };
  const photos = new Photos("scoped", vi.fn());
  const preview = vi.spyOn(photos, "preview").mockImplementation(async (_item, size) => {
    if (size === "large" && preview.mock.calls.filter(([, s]) => s === "large").length === 1)
      throw new Error("Preview is still being prepared");
    return { state: "ready", generation_id: `${size}-1` };
  });
  const get = vi.fn(async (_asset: string, _generation: string) => new Blob(["synthetic-jpeg"]));
  const view = { ...fittedPhotoView };
  const { container } = render(PhotoStage, {
    photo: item,
    photos,
    cache: { get } as unknown as PhotoPreviewCache,
    view,
  });
  await waitFor(() => expect(preview).toHaveBeenCalledWith(item, "fit", expect.anything()));
  await waitFor(() => expect(container.querySelector("img")).toBeTruthy());
  await waitFor(() =>
    expect(get).toHaveBeenCalledWith(item.asset_id, "fit-1", expect.anything(), false),
  );
  const fitted = container.querySelector("img")!.getAttribute("src");
  const viewport = container.querySelector<HTMLElement>(".photo-viewport")!;
  await fireEvent.doubleClick(viewport);
  await screen.findByText("Preview is still being prepared");
  expect(container.querySelector("img")!.getAttribute("src")).toBe(fitted);
  await fireEvent.click(screen.getByRole("button", { name: "Retry preview" }));
  await waitFor(() =>
    expect(preview.mock.calls.filter(([, size]) => size === "large")).toHaveLength(2),
  );
  await waitFor(() => expect(screen.queryByText("Preview is still being prepared")).toBeNull());
  await waitFor(() => expect(container.querySelector("img")!.getAttribute("src")).not.toBe(fitted));
});

it.each(["rejected", "missing"] as const)(
  "keeps a successful large preview free of errors when the pending fit preview is %s",
  async (failure) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic-large");
    vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
    Object.defineProperty(HTMLImageElement.prototype, "decode", {
      configurable: true,
      value: vi.fn(async () => {}),
    });
    let rejectFit!: (cause: Error) => void;
    let resolveFit!: (slot: PhotoPreviewSlot) => void;
    const fit = new Promise<PhotoPreviewSlot>((resolve, reject) => {
      rejectFit = reject;
      resolveFit = resolve;
    });
    const item = photo(1);
    item.previews.grid = { state: "missing" };
    const photos = new Photos("scoped", vi.fn());
    const preview = vi
      .spyOn(photos, "preview")
      .mockImplementation((_item, size) =>
        size === "fit" ? fit : Promise.resolve({ state: "ready", generation_id: "large-1" }),
      );
    const { container } = render(PhotoStage, {
      photo: item,
      photos,
      cache: {
        get: vi.fn(async () => new Blob(["synthetic-large"])),
      } as unknown as PhotoPreviewCache,
      view: { ...fittedPhotoView },
    });
    await waitFor(() => expect(preview).toHaveBeenCalledWith(item, "fit", expect.anything()));
    await fireEvent.doubleClick(container.querySelector<HTMLElement>(".photo-viewport")!);
    await waitFor(() =>
      expect(container.querySelector("img")?.getAttribute("src")).toBe("blob:synthetic-large"),
    );
    if (failure === "rejected") rejectFit(new Error("Fit preview failed"));
    else resolveFit({ state: "missing" });
    await fit.catch(() => {});
    await tick();
    expect(container.querySelector("img")?.getAttribute("src")).toBe("blob:synthetic-large");
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry preview" })).toBeNull();
  },
);

it("offers Retry preview after the fit preview fails and shows the photo once the server recovers", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic-fit");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: vi.fn(async () => {}),
  });
  const item = photo(1);
  item.previews.grid = { state: "missing" };
  const photos = new Photos("scoped", vi.fn());
  const preview = vi.spyOn(photos, "preview").mockImplementation(async () => {
    if (preview.mock.calls.length === 1) throw new Error("Preview is still being prepared");
    return { state: "ready", generation_id: "fit-1" };
  });
  const get = vi.fn(async () => new Blob(["synthetic-jpeg"]));
  const { container } = render(PhotoStage, {
    photo: item,
    photos,
    cache: { get } as unknown as PhotoPreviewCache,
    view: { ...fittedPhotoView },
  });
  await screen.findByText("Preview is still being prepared");
  expect(container.querySelector("img")).toBeNull();
  await fireEvent.click(await screen.findByRole("button", { name: "Retry preview" }));
  await waitFor(() => expect(container.querySelector("img")).toBeTruthy());
  expect(get).toHaveBeenCalledWith(item.asset_id, "fit-1", expect.anything(), true);
  expect(screen.queryByText("Preview is still being prepared")).toBeNull();
});

it.each([
  { removed: false, changed: false },
  { removed: true, changed: false },
  { removed: false, changed: true },
])(
  "refreshes a stale preview and checks whether the asset was removed: %j",
  async ({ removed, changed }) => {
    vi.stubGlobal(
      "ResizeObserver",
      class {
        observe() {}
        disconnect() {}
      },
    );
    vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic-current");
    vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
    Object.defineProperty(HTMLImageElement.prototype, "decode", {
      configurable: true,
      value: vi.fn(async () => {}),
    });
    const item = photo(1);
    item.previews.grid = { state: "missing" };
    const current = {
      ...item,
      content_version_id: "version-2",
      previews: {
        ...item.previews,
        fit: { state: "ready" as const, generation_id: "current-fit" },
      },
    };
    const photos = new Photos("scoped", vi.fn());
    const lookup = vi.spyOn(photos, "lookup").mockImplementation(async () => {
      if (removed) throw new APIError("This photo is no longer in this view.", 404, "not_found");
      return current;
    });
    const get = vi.fn(async (_id: string, generation: string) => {
      if (generation !== "current-fit") throw new APIError("Preview not found", 404, "not_found");
      return new Blob(["synthetic-photo"]);
    });
    vi.spyOn(photos, "preview").mockImplementation(async (row) => {
      if (row === current) return current.previews.fit;
      if (changed) throw new APIError("Photo display changed", 409, "photo_display_changed");
      return { state: "ready", generation_id: "expired-fit" };
    });
    const onreload = vi.fn(),
      onunavailable = vi.fn();
    const { container } = render(PhotoStage, {
      photo: item,
      photos,
      cache: { get } as unknown as PhotoPreviewCache,
      view: { ...fittedPhotoView },
      onreload,
      onunavailable,
    });
    await screen.findByText(changed ? "Photo display changed" : "Preview not found");
    await fireEvent.click(screen.getByRole("button", { name: "Retry preview" }));
    if (removed) {
      await screen.findByText("This photo is no longer in this view.");
      expect(container.querySelector("img")).toBeNull();
      expect(onunavailable).toHaveBeenCalledWith("This photo is no longer in this view.");
      expect(onreload).not.toHaveBeenCalled();
      expect(get).toHaveBeenCalledTimes(1);
    } else {
      await waitFor(() => expect(container.querySelector("img")).toBeTruthy());
      expect(onreload).toHaveBeenCalledWith(current);
      expect(onunavailable).not.toHaveBeenCalled();
    }
    expect(lookup).toHaveBeenCalledWith(item.asset_id, expect.anything());
  },
);
