import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, render, waitFor } from "@testing-library/svelte";
import PhotoAlbumCover from "./PhotoAlbumCover.svelte";
import { photoAlbum } from "./photo-test-fixtures.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
it("updates the cover rotation", async () => {
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: vi.fn(() => "blob:cover"),
  });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  const get = vi.fn(async (_asset: string, _generation: string) => new Blob(["synthetic JPEG"]));
  const cache = { get } as unknown as PhotoPreviewCache;
  const album = photoAlbum({
    effective_cover_asset_id: "asset",
    cover_generation_id: "generation",
    cover_rotation: 0,
  });
  const view = render(PhotoAlbumCover, { album, cache });
  await waitFor(() => expect(view.container.querySelector("img")?.src).toBe("blob:cover"));
  await view.rerender({ album: { ...album, cover_rotation: 90 }, cache });
  expect(view.container.querySelector("img")?.style.transform).toBe("rotate(90deg)");
});
