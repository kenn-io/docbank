import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoStage from "./PhotoStage.svelte";
import { Photos } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import { fittedPhotoView } from "./photoView.js";
import { photo, storage } from "./photo-test-fixtures.js";
import { APIError } from "./api-transport.js";

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
  expect(screen.queryByText("Preview is still being prepared")).toBeNull();
});

it("refetches a fit preview that failed to decode instead of replaying its cached bytes", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  storage();
  const sources = new Map<string, Blob>();
  let counter = 0;
  vi.spyOn(URL, "createObjectURL").mockImplementation((source) => {
    const url = `blob:synthetic-${++counter}`;
    sources.set(url, source as Blob);
    return url;
  });
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: vi.fn(async function (this: HTMLImageElement) {
      if (sources.get(this.src)?.size === "undecodable".length) throw new Error("Decode failed");
    }),
  });
  const fetched = vi
    .fn()
    .mockResolvedValueOnce(new Response("undecodable"))
    .mockResolvedValueOnce(new Response("repaired"));
  vi.stubGlobal("fetch", fetched);
  const item = photo(1);
  item.previews.grid = { state: "missing" };
  const photos = new Photos("scoped", vi.fn());
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "ready", generation_id: "fit-1" });
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  try {
    const { container } = render(PhotoStage, {
      photo: item,
      photos,
      cache,
      view: { ...fittedPhotoView },
    });
    await screen.findByText("Decode failed");
    expect(container.querySelector("img")).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: "Retry preview" }));
    await waitFor(() => expect(container.querySelector("img")).toBeTruthy());
    expect(screen.queryByText("Decode failed")).toBeNull();
    expect(fetched).toHaveBeenCalledTimes(2);
  } finally {
    await cache.dispose();
  }
});

it("does not resend a stale content version when the server reports the photo display changed", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  // The server's preparePhotoPreview route answers 409 photo_display_changed unless the request names the asset's current content version.
  const currentVersion = "version-2";
  const prepared: string[] = [];
  const current = { ...photo(1), content_version_id: currentVersion };
  vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:synthetic-current");
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: vi.fn(async () => {}),
  });
  const fetched = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    if (String(url).endsWith("/photos/assets/query")) return Response.json({ items: [current] });
    const { content_version_id } = JSON.parse(String(init?.body));
    prepared.push(content_version_id);
    if (content_version_id === currentVersion)
      return Response.json({ state: "ready", generation_id: "fit-2" });
    return Response.json(
      {
        title: "Conflict",
        status: 409,
        detail: "The photo display changed. Refresh Library.",
        code: "photo_display_changed",
      },
      { status: 409 },
    );
  });
  vi.stubGlobal("fetch", fetched);
  const item = photo(1);
  item.previews.grid = { state: "missing" };
  expect(item.content_version_id).not.toBe(currentVersion);
  const photos = new Photos("scoped", vi.fn());
  render(PhotoStage, {
    photo: item,
    photos,
    cache: {
      get: vi.fn(async () => new Blob(["synthetic-photo"])),
    } as unknown as PhotoPreviewCache,
    view: { ...fittedPhotoView },
  });
  await screen.findByText(/The photo display changed/);
  expect(prepared).toEqual([item.content_version_id]);
  await fireEvent.click(screen.getByRole("button", { name: "Retry preview" }));
  await waitFor(() => expect(document.querySelector("img")).toBeTruthy());
  expect(prepared).toEqual([item.content_version_id, currentVersion]);
});

it.each([false, true])(
  "refreshes a missing preview generation and checks whether the asset was removed: %s",
  async (removed) => {
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
    vi.spyOn(photos, "preview").mockImplementation(async (row) =>
      row === current ? current.previews.fit : { state: "ready", generation_id: "expired-fit" },
    );
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
    await screen.findByText("Preview not found");
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
