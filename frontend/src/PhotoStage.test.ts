import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoStage from "./PhotoStage.svelte";
import { Photos } from "./photos.svelte.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";
import { fittedPhotoView } from "./photoView.js";
import { photo } from "./photo-test-fixtures.js";

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
