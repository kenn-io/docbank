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
