import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoStage from "./PhotoStage.svelte";
import { Photos } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import { fittedPhotoView } from "./photoView.js";
import { photo } from "./photo-test-fixtures.js";
import { APIError } from "./api-transport.js";

afterEach(() => {
  cleanup();
  // jsdom has no HTMLImageElement.decode, so the test defines it and this removes it again.
  delete (HTMLImageElement.prototype as { decode?: unknown }).decode;
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
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

it("keeps the large preview when a replaced row loads while zoomed to 1:1", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  vi.spyOn(URL, "createObjectURL").mockImplementation(
    (blob) => `blob:${(blob as Blob & { id: string }).id}`,
  );
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: async () => {},
  });
  const item = photo(1);
  item.previews.grid = { state: "ready", generation_id: "grid-1" };
  item.previews.fit = { state: "ready", generation_id: "fit-1" };
  const photos = new Photos("scoped", vi.fn());
  const preview = vi
    .spyOn(photos, "preview")
    .mockImplementation(async (row, size) =>
      size === "fit"
        ? row.previews.fit
        : { state: "ready", generation_id: `large-of-${row.previews.fit.generation_id}` },
    );
  const cache = {
    get: vi.fn(async (_asset: string, id: string) =>
      Object.assign(new Blob(["synthetic-photo"]), { id }),
    ),
  } as unknown as PhotoPreviewCache;
  const props = { photo: item, photos, cache, view: { ...fittedPhotoView } };
  const { container, rerender } = render(PhotoStage, props);
  const source = () => container.querySelector("img")?.getAttribute("src");
  await waitFor(() => expect(source()).toBe("blob:fit-1"));
  container.querySelector<HTMLElement>(".photo-viewport")!.dataset.zoomed = "true";
  await waitFor(() => expect(source()).toBe("blob:large-of-fit-1"));
  const replaced = {
    ...item,
    previews: { ...item.previews, fit: { state: "ready" as const, generation_id: "fit-2" } },
  };
  await rerender({ ...props, photo: replaced });
  await waitFor(() => expect(source()).toBe("blob:large-of-fit-2"));
  expect(preview).toHaveBeenCalledWith(replaced, "large", expect.anything());
});
