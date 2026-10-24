import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { PhotoPreviewLoader, prefetchPreviews } from "./photoPreview.svelte.js";
import { Photos } from "./photos.svelte.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";
import type { PhotoPreviewSlot } from "./generated/docbank.js";
import { photo } from "./photo-test-fixtures.js";

const displays: PhotoPreviewLoader[] = [];
beforeEach(() => {
  let counter = 0;
  vi.spyOn(URL, "createObjectURL").mockImplementation(() => `blob:preview-${++counter}`);
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  vi.spyOn(HTMLImageElement.prototype, "naturalWidth", "get").mockReturnValue(1200);
  vi.spyOn(HTMLImageElement.prototype, "naturalHeight", "get").mockReturnValue(800);
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: async () => {},
  });
});
afterEach(() => {
  for (const display of displays.splice(0)) display.dispose();
  delete (HTMLImageElement.prototype as { decode?: unknown }).decode;
  vi.restoreAllMocks();
});
function loader(photos: Photos, row = photo(1)) {
  const get = vi.fn(async () => new Blob(["synthetic-preview"]));
  const display = new PhotoPreviewLoader(photos, { get } as unknown as PhotoPreviewCache, row);
  displays.push(display);
  return { display, get };
}

it.each(["fit", "large"] as const)(
  "retries a failed %s preview on the same photo",
  async (size) => {
    const photos = new Photos("session", vi.fn());
    const preview = vi.spyOn(photos, "preview").mockImplementation(async (_row, requested) => {
      if (
        requested === size &&
        preview.mock.calls.filter(([, value]) => value === size).length === 1
      )
        throw new Error("Preview is still being prepared");
      return { state: "ready", generation_id: `${requested}-1` };
    });
    const { display, get } = loader(photos);
    if (size === "large") {
      await vi.waitFor(() => expect(display.quality).toBe("fit"));
      display.zoom();
    }
    await vi.waitFor(() =>
      expect(display.failure?.message).toBe("Preview is still being prepared"),
    );
    const fitted = display.url;
    if (size === "large") expect(display.quality).toBe("fit");
    await display.retry();
    expect(display.failure).toBeUndefined();
    expect(display.quality).toBe(size);
    expect(display.url).not.toBe(fitted);
    expect(get).toHaveBeenLastCalledWith("photo-1", `${size}-1`, expect.anything(), true);
  },
);

it("retries a rejected fit preview while preserving the successful large preview", async () => {
  const photos = new Photos("session", vi.fn());
  let rejectFit!: (cause: Error) => void;
  const fit = new Promise<PhotoPreviewSlot>((_resolve, reject) => {
    rejectFit = reject;
  });
  const preview = vi
    .spyOn(photos, "preview")
    .mockImplementation((_row, size) =>
      size === "fit" ? fit : Promise.resolve({ state: "ready", generation_id: "large-1" }),
    );
  const { display, get } = loader(photos);
  await vi.waitFor(() => expect(preview).toHaveBeenCalled());
  display.zoom();
  await vi.waitFor(() => expect(display.quality).toBe("large"));
  const url = display.url;
  rejectFit(new Error("Fit preview failed"));
  await fit.catch(() => {});
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(display.url).toBe(url);
  expect(display.failure).toBeUndefined();
  expect(display.fit).toBeUndefined();
  await vi.waitFor(() => expect(display.fitFailure?.action).toBe("fit"));
  preview.mockResolvedValueOnce({ state: "ready", generation_id: "fit-retry" });
  await display.retry(display.fitFailure?.action);
  expect(display.fitFailure).toBeUndefined();
  expect(display.fit).toEqual({
    url: "blob:preview-2",
    content_version_id: "version-1",
    width: 1200,
    height: 800,
  });
  expect(get).toHaveBeenLastCalledWith("photo-1", "fit-retry", expect.anything(), true);
  expect(display.url).toBe(url);
  expect(display.quality).toBe("large");
});

it("reports unsupported previews without offering an ineffective retry", async () => {
  const photos = new Photos("session", vi.fn());
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  const { display } = loader(photos);
  await vi.waitFor(() =>
    expect(display.failure?.message).toBe(
      "Preview unsupported. Download this photo from Documents.",
    ),
  );
  expect(display.failure?.action).toBeUndefined();
});

it("prefetches fit previews for the supplied rail rows and stops on disposal", async () => {
  const photos = new Photos("session", vi.fn()),
    rows = [photo(1), photo(2)];
  const detail = vi
    .spyOn(photos, "detail")
    .mockImplementation(async (row) => row as ReturnType<typeof photo>);
  const preview = vi
    .spyOn(photos, "preview")
    .mockResolvedValue({ state: "ready", generation_id: "fit-1" });
  const get = vi.fn(async () => new Blob(["synthetic-preview"]));
  const stop = prefetchPreviews(photos, { get } as unknown as PhotoPreviewCache, rows);
  await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(2));
  stop();
  expect(detail.mock.calls.map(([row]) => row.asset_id)).toEqual(["photo-1", "photo-2"]);
  expect(preview.mock.calls.map(([, size]) => size)).toEqual(["fit", "fit"]);
  const cancel = prefetchPreviews(photos, { get } as unknown as PhotoPreviewCache, [photo(3)]);
  cancel();
  await new Promise((resolve) => setTimeout(resolve, 175));
  expect(get).toHaveBeenCalledTimes(2);
});
