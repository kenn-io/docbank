import { fromStore, writable } from "svelte/store";
import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { tick } from "svelte";
import { APIError } from "./api-transport.js";
import PhotoLoupe from "./PhotoLoupe.svelte";
import PhotoLoupeImage from "./PhotoLoupeImage.svelte";
import { Photos } from "./photos.svelte.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";
import { photo, mapPhoto } from "./photo-test-fixtures.js";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});
function setup() {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  let counter = 0;
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: vi.fn(() => `blob:synthetic-${++counter}`),
  });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  Object.defineProperty(HTMLImageElement.prototype, "decode", {
    configurable: true,
    value: vi.fn(async () => {}),
  });
  const items = Array.from({ length: 9 }, (_, index) => {
    const item = photo(index + 1);
    item.previews.grid = { state: "ready", generation_id: `grid-${index + 1}` };
    return item;
  });
  const photos = new Photos("scoped", vi.fn());
  photos.items = items;
  photos.total = 9;
  const preview = vi.spyOn(photos, "preview").mockImplementation(async (item, size) => ({
    state: "ready",
    generation_id: `${size}-${item.node_id}`,
  }));
  const get = vi.fn(
    async (_asset: string, _generation: string, _signal?: AbortSignal) =>
      new Blob(["synthetic-jpeg"]),
  );
  const props = {
    photo: items[4]!,
    error: "",
    photos,
    cache: { get } as unknown as PhotoPreviewCache,
    items,
    canLoadMore: false,
    onmove: vi.fn(),
    onchoose: vi.fn(),
    onclose: vi.fn(),
    onretry: vi.fn(),
  };
  return { props, preview, get };
}
it("shows grid, fit and large and preserves zoom", async () => {
  const { props, preview } = setup();
  const currentPhoto = fromStore(writable(props.photo));
  const reloadRevision = fromStore(writable(0));
  const view = render(PhotoLoupe, {
    ...props,
    get photo() {
      return currentPhoto.current;
    },
    get reloadRevision() {
      return reloadRevision.current;
    },
  });
  await screen.findByText("Grid preview");
  await screen.findByText("Fit preview");
  reloadRevision.current = 1;
  await waitFor(() =>
    expect(
      preview.mock.calls.filter(([item, size]) => item.node_id === 5 && size === "fit"),
    ).toHaveLength(2),
  );
  expect(preview.mock.calls.filter(([, size]) => size === "large")).toHaveLength(0);
  const viewport = view.container.querySelector<HTMLElement>(".photo-viewport")!;
  const pan = viewport.querySelector<HTMLElement>(".pan")!;
  await fireEvent.doubleClick(viewport, { clientX: 0, clientY: 0 });
  const transform = pan.style.transform;
  await screen.findByText("Large preview");
  expect(viewport.querySelector(".pan")).toBe(pan);
  expect(pan.style.transform).toBe(transform);
  await fireEvent.doubleClick(viewport);
  await fireEvent.doubleClick(viewport);
  expect(preview.mock.calls.filter(([, size]) => size === "large")).toHaveLength(1);
  reloadRevision.current = 2;
  await waitFor(() =>
    expect(preview.mock.calls.filter(([, size]) => size === "large")).toHaveLength(2),
  );
  await screen.findByText("Large preview");
  await waitFor(() =>
    expect(
      preview.mock.calls.filter(([item, size]) => item.node_id === 5 && size === "fit"),
    ).toHaveLength(3),
  );
  await tick();
  expect(screen.getByText("Large preview")).toBeTruthy();
  expect(view.container.querySelector(".photo-viewport")).toBe(viewport);
  expect(pan.style.transform).toBe(transform);
  currentPhoto.current = { ...props.photo, content_version_id: "fresh-version" };
  await screen.findByText("Fit preview");
  expect(preview.mock.calls.filter(([, size]) => size === "large")).toHaveLength(2);
  expect(view.container.querySelector(".photo-viewport")).not.toBe(viewport);
  expect(view.container.querySelector<HTMLElement>(".pan")!.style.transform).toBe(
    "translate(0px, 0px) scale(1)",
  );
  await fireEvent.keyDown(screen.getByRole("dialog"), { key: "ArrowRight" });
  expect(props.onmove).toHaveBeenCalledWith(1);
  await fireEvent.doubleClick(view.container.querySelector(".photo-viewport")!);
  await screen.findByText("Large preview");
});

it("ignores late image bytes after navigation or unmount", async () => {
  const { props, preview, get } = setup();
  let finish!: (blob: Blob) => void;
  get.mockImplementationOnce(() => new Promise((resolve) => (finish = resolve)));
  const view = render(PhotoLoupe, { ...props, items: [] });
  await waitFor(() => expect(get).toHaveBeenCalled());
  await view.rerender({ ...props, photo: props.items[5]!, items: [] });
  finish(new Blob(["late"]));
  await screen.findByText("Fit preview");
  expect(view.container.querySelector(".photo-viewport img")?.getAttribute("alt")).toBe(
    "Photo 6.jpg",
  );
  expect(URL.createObjectURL).toHaveBeenCalledTimes(2);
  expect(screen.queryByRole("navigation", { name: "Photos in current result" })).toBeNull();
  let finishUnmounted!: (blob: Blob) => void;
  let unmountedSignal!: AbortSignal;
  get.mockImplementationOnce((_asset, _generation, signal) => {
    unmountedSignal = signal!;
    return new Promise((resolve) => (finishUnmounted = resolve));
  });
  const pending = view.rerender({ ...props, photo: props.items[5]!, items: [], reloadRevision: 1 });
  await waitFor(() => expect(finishUnmounted).toBeTypeOf("function"));
  view.unmount();
  await pending;
  expect(unmountedSignal.aborted).toBe(true);
  const createdURLs = vi.mocked(URL.createObjectURL).mock.calls.length;
  const previewRequests = preview.mock.calls.length;
  finishUnmounted(new Blob(["late"]));
  await tick();
  expect(URL.createObjectURL).toHaveBeenCalledTimes(createdURLs);
  expect(preview).toHaveBeenCalledTimes(previewRequests);
});

it("shows paging recovery and preserves fractional exposure with readable orientation", async () => {
  const { props } = setup();
  props.photos.error = "Photo continuation expired";
  props.photo.exposure_time_seconds = 0.4;
  props.photo.orientation = 6;
  props.photo.iso = 640;
  props.photo.f_number = 3.2;
  props.photo.exposure_bias_ev = -1.5;
  props.photo.focal_length_mm = 85;
  const view = render(PhotoLoupe, { ...props, metadataOpen: true });
  await screen.findByText("Photo continuation expired");
  await fireEvent.click(screen.getByRole("button", { name: "Retry navigation" }));
  expect(props.onretry).toHaveBeenCalledOnce();
  expect(screen.getByText("0.4 s")).toBeTruthy();
  expect(screen.getByText("Rotated 90° clockwise")).toBeTruthy();
  for (const value of ["640", "f/3.2", "-1.5 EV", "85 mm"])
    expect(screen.getByText(value)).toBeTruthy();
  for (const [seconds, label] of [
    [0.004, "1/250 s"],
    [0.000125, "1/8000 s"],
    [0.4, "0.4 s"],
    [0.0167, "0.0167 s"],
    [2, "2 s"],
  ] as const) {
    await view.rerender({ ...props, photo: { ...props.photo, exposure_time_seconds: seconds } });
    expect(screen.getByText(label)).toBeTruthy();
  }
});

it.each(["preview", "cache"])(
  "retains Fit after a %s failure, retries Large in place and reuses it on zoom",
  async (source) => {
    const { props, preview, get } = setup();
    const note = "This photo is no longer in the refreshed result.";
    const view = render(PhotoLoupe, { ...props, items: [], note });
    await screen.findByText("Fit preview");
    expect(screen.getByRole("alert").textContent).toContain(note);
    expect(screen.getByRole("button", { name: "Reload photo" })).toBeTruthy();
    if (source === "preview") preview.mockRejectedValueOnce(new Error("Large unavailable"));
    else get.mockRejectedValueOnce(new Error("Large unavailable"));
    const viewport = view.container.querySelector(".photo-viewport")!;
    await fireEvent.doubleClick(viewport);
    await screen.findByText("Large unavailable");
    expect(screen.getByText("Fit preview")).toBeTruthy();
    if (source === "preview") {
      await fireEvent.doubleClick(viewport);
      await fireEvent.doubleClick(viewport);
    } else await fireEvent.click(screen.getByRole("button", { name: "Retry preview" }));
    await screen.findByText("Large preview");
    expect(screen.queryByText("Large unavailable")).toBeNull();
    const created = vi.mocked(URL.createObjectURL).mock.calls.length;
    await fireEvent.doubleClick(viewport);
    await fireEvent.doubleClick(viewport);
    await new Promise((resolve) => setTimeout(resolve, 0));
    await tick();
    expect(preview.mock.calls.filter(([, size]) => size === "large")).toHaveLength(2);
    expect(get.mock.calls.filter(([, generation]) => generation === "large-5")).toHaveLength(
      source === "preview" ? 1 : 2,
    );
    expect(get).toHaveBeenLastCalledWith(
      props.photo.asset_id,
      "large-5",
      expect.any(AbortSignal),
      source === "cache",
    );
    expect(URL.createObjectURL).toHaveBeenCalledTimes(created);
  },
);

it.each(["photo", "reload"])(
  "keeps a newer retry busy after an obsolete retry settles on %s",
  async (replacement) => {
    const { props, preview, get } = setup();
    get.mockRejectedValue(new Error("Grid failed"));
    preview.mockRejectedValue(new Error("Fit failed"));
    const view = render(PhotoLoupe, { ...props, items: [], reloading: true });
    await screen.findByText("Fit failed");
    expect(screen.getByText("No preview")).toBeTruthy();
    expect(
      (screen.getByRole("button", { name: "Retry preview" }) as HTMLButtonElement).disabled,
    ).toBe(true);
    await view.rerender({ ...props, items: [], reloading: false });
    await screen.findByText("Fit failed");
    let finishOld!: (slot: { state: "ready"; generation_id: string }) => void,
      rejectNew!: (cause: Error) => void;
    preview.mockImplementationOnce(() => new Promise((resolve) => (finishOld = resolve)));
    await fireEvent.click(screen.getByRole("button", { name: "Retry preview" }));
    expect(screen.getByText("Loading preview…")).toBeTruthy();
    const changed = {
      ...props,
      items: [],
      photo: replacement === "photo" ? props.items[5]! : props.photo,
      reloadRevision: replacement === "reload" ? 1 : 0,
    };
    await view.rerender(changed);
    await screen.findByText("Fit failed");
    preview.mockImplementationOnce(() => new Promise((_, fail) => (rejectNew = fail)));
    await fireEvent.click(screen.getByRole("button", { name: "Retry preview" }));
    finishOld({ state: "ready", generation_id: "obsolete" });
    await new Promise((resolve) => setTimeout(resolve, 0));
    await tick();
    const retry = screen.getByRole("button", { name: "Retry preview" }) as HTMLButtonElement;
    expect(retry.disabled).toBe(true);
    expect(screen.getByText("Loading preview…")).toBeTruthy();
    rejectNew(new Error("Retry failed"));
    await screen.findByText("Retry failed");
    await waitFor(() => expect(retry.disabled).toBe(false));
    expect(screen.getByText("No preview")).toBeTruthy();
    get.mockResolvedValue(new Blob(["synthetic"]));
    preview.mockResolvedValue({ state: "ready", generation_id: "recovered" });
    await fireEvent.click(screen.getByRole("button", { name: "Retry preview" }));
    await screen.findByText("Fit preview");
    expect(screen.queryByRole("alert")).toBeNull();
  },
);

it.each(["preview", "original"] as const)(
  "releases each rejected %s URL before retry or close",
  async (operation) => {
    const { props, get } = setup();
    get.mockImplementation(
      async (_asset, generation, _signal?: AbortSignal, reload = false) =>
        new Blob([generation.startsWith("fit") && !reload ? "broken" : "valid"]),
    );
    const createURL = vi.mocked(URL.createObjectURL).getMockImplementation()!,
      brokenURLs = new Set<string>();
    vi.mocked(URL.createObjectURL).mockImplementation((blob) => {
      const url = createURL(blob);
      if (blob instanceof Blob && blob.size === 6) brokenURLs.add(url);
      return url;
    });
    vi.spyOn(props.photos, "original").mockResolvedValue("blob:broken-original");
    vi.spyOn(HTMLImageElement.prototype, "decode").mockImplementation(
      function (this: HTMLImageElement) {
        return this.src === "blob:synthetic-1"
          ? Promise.resolve()
          : Promise.reject(new Error("Decode failed"));
      },
    );
    const view = render(PhotoLoupe, { ...props, items: [] });
    await screen.findByText("Grid preview");
    await screen.findByText("Decode failed");
    for (let attempt = 0; attempt < 3; attempt++) {
      const button = await screen.findByRole("button", {
        name: operation === "preview" ? "Retry preview" : "View original",
      });
      await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(false));
      await fireEvent.click(button);
      const objectURL =
        operation === "preview" ? `blob:synthetic-${attempt + 3}` : "blob:broken-original";
      await waitFor(() =>
        expect(
          vi.mocked(URL.revokeObjectURL).mock.calls.filter(([url]) => url === objectURL),
        ).toHaveLength(operation === "preview" ? 1 : attempt + 1),
      );
    }
    if (operation === "preview") {
      expect(get).toHaveBeenLastCalledWith(
        props.photo.asset_id,
        "fit-5",
        expect.any(AbortSignal),
        true,
      );
      vi.mocked(HTMLImageElement.prototype.decode).mockImplementation(
        function (this: HTMLImageElement) {
          return brokenURLs.has(this.src)
            ? Promise.reject(new Error("Decode failed"))
            : Promise.resolve();
        },
      );
      await fireEvent.click(screen.getByRole("button", { name: "Retry preview" }));
      await screen.findByText("Fit preview");
      expect(screen.queryByText("Decode failed")).toBeNull();
    }
    const released = vi.mocked(URL.revokeObjectURL).mock.calls.length;
    view.unmount();
    expect(URL.revokeObjectURL).toHaveBeenCalledTimes(released + (operation === "preview" ? 2 : 1));
  },
);

it.each([false, true])(
  "serializes neighboring fit requests and hydrates compact neighbors, compact=%s",
  async (compact) => {
    const { props, preview } = setup();
    let release!: () => void;
    preview.mockImplementation(async (item, size) => {
      if (item.asset_id === "photo-3") await new Promise<void>((resolve) => (release = resolve));
      return { state: "ready", generation_id: `${size}-${item.node_id}` };
    });
    const lookup = vi
      .spyOn(props.photos, "lookup")
      .mockImplementation(async (id) => photo(Number(id.split("-")[1])));
    const items = compact
      ? props.items.map((item, index) => ({
          ...mapPhoto(index + 1),
          previews: { grid: item.previews.grid },
        }))
      : props.items;
    render(PhotoLoupe, { ...props, items });
    await screen.findByText("Fit preview");
    await waitFor(() => expect(release).toBeTypeOf("function"));
    expect(preview.mock.calls.map(([item]) => item.asset_id)).toEqual(["photo-5", "photo-3"]);
    release();
    await waitFor(() => expect(preview).toHaveBeenCalledTimes(5));
    expect(preview.mock.calls.map(([item, size]) => [item.asset_id, size])).toEqual(
      ["photo-5", "photo-3", "photo-4", "photo-6", "photo-7"].map((id) => [id, "fit"]),
    );
    expect(lookup.mock.calls.map(([id]) => id)).toEqual(
      compact ? ["photo-3", "photo-4", "photo-6", "photo-7"] : [],
    );
  },
);

it("resets a touch swipe before requesting navigation when the current photo remains mounted", async () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const onmove = vi.fn(() => expect(pan.style.transform).toBe("translate(0px, 0px) scale(1)"));
  const view = render(PhotoLoupeImage, {
    url: "",
    name: "Synthetic photo",
    onzoom: vi.fn(),
    onmove,
  });
  const viewport = view.container.querySelector<HTMLElement>(".photo-viewport")!;
  const pan = viewport.querySelector<HTMLElement>(".pan")!;
  for (const [type, x] of [
    ["pointerdown", 200],
    ["pointermove", 50],
    ["pointerup", 50],
  ] as const) {
    const event = new MouseEvent(type, { clientX: x, clientY: 100, bubbles: true, button: 0 });
    Object.defineProperties(event, { pointerType: { value: "touch" }, pointerId: { value: 1 } });
    await fireEvent(viewport, event);
  }
  expect(onmove).toHaveBeenCalledWith(1);
});

it.each(["failed", "unsupported", "retained"] as const)(
  "shows a terminal %s fit without Retry or an endless loading placeholder",
  async (scenario) => {
    const { props, preview } = setup();
    const state = scenario === "retained" ? "failed" : scenario;
    if (scenario !== "retained") props.photo.previews.grid = { state };
    preview.mockResolvedValue({ state });
    const view = render(PhotoLoupe, { ...props, items: [] });
    if (scenario === "retained") {
      await screen.findByText(
        "Preview unavailable for this version. Download this photo from Documents.",
      );
      expect(screen.getByText("Grid preview")).toBeTruthy();
    } else await screen.findByText("No preview");
    expect(screen.queryByText("Loading preview…")).toBeNull();
    expect(screen.queryByRole("button", { name: "Retry preview" })).toBeNull();
    await fireEvent.doubleClick(view.container.querySelector(".photo-viewport")!);
    await waitFor(() =>
      expect(preview.mock.calls.filter(([, size]) => size === "large")).toHaveLength(1),
    );
    await fireEvent.doubleClick(view.container.querySelector(".photo-viewport")!);
    await fireEvent.doubleClick(view.container.querySelector(".photo-viewport")!);
    expect(preview.mock.calls.filter(([, size]) => size === "large")).toHaveLength(1);
  },
);

it.each([false, true])(
  "clears a grid failure when fit displays, delayed grid %s",
  async (delayed) => {
    const { props, get } = setup();
    let reject!: (cause: Error) => void;
    get.mockImplementationOnce(() => new Promise((_, fail) => (reject = fail)));
    let finish!: (blob: Blob) => void;
    if (!delayed) get.mockImplementationOnce(() => new Promise((resolve) => (finish = resolve)));
    render(PhotoLoupe, { ...props, items: [] });
    await waitFor(() => expect(reject).toBeTypeOf("function"));
    if (!delayed) {
      reject(new Error("Grid failed"));
      await screen.findByText("Grid failed");
      await waitFor(() => expect(finish).toBeTypeOf("function"));
      expect(screen.getByText("Grid failed")).toBeTruthy();
      finish(new Blob(["synthetic-fit"]));
    }
    await screen.findByText("Fit preview");
    if (delayed) {
      reject(new Error("Grid failed"));
      await new Promise((resolve) => setTimeout(resolve, 0));
      await tick();
    }
    expect(screen.queryByRole("alert")).toBeNull();
  },
);

it.each(["generation", "preparation"])("reloads a fit whose %s returns 404", async (stage) => {
  const { props, get, preview } = setup();
  props.photo.previews.fit = { state: "ready", generation_id: "old-fit" };
  preview.mockResolvedValue(props.photo.previews.fit);
  get.mockImplementation(async (_, generation) => {
    if (generation === "old-fit") throw new APIError("Preview unavailable", 404, "not_found");
    return new Blob(["synthetic"]);
  });
  if (stage === "preparation") {
    props.photo.previews.fit = { state: "missing" };
    preview.mockRejectedValue(new APIError("Preview unavailable", 404, "not_found"));
  }
  const onreload = vi.fn();
  render(PhotoLoupe, { ...props, items: [], onreload });
  await screen.findByText("Preview unavailable");
  expect(screen.queryByRole("button", { name: "Retry preview" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Reload photo" }));
  expect(onreload).toHaveBeenCalledOnce();
});

it.each(["success", "failure", "terminal", "fit-first"] as const)(
  "clears preview failures on successful original recovery after %s",
  async (outcome) => {
    const { props, preview } = setup();
    let finish!: (slot: { state: "ready" | "failed"; generation_id?: string }) => void,
      reject!: (cause: Error) => void;
    preview.mockImplementationOnce(
      () =>
        new Promise((resolve, fail) => {
          finish = resolve;
          reject = fail;
        }),
    );
    const original = vi.spyOn(props.photos, "original");
    if (outcome !== "fit-first") original.mockRejectedValueOnce(new Error("Original failed"));
    original.mockResolvedValueOnce("blob:original");
    render(PhotoLoupe, { ...props, items: [] });
    await screen.findByText("Grid preview");
    await waitFor(() => expect(finish).toBeTypeOf("function"));
    if (outcome === "fit-first") {
      reject(new Error("Fit failed"));
      await screen.findByText("Fit failed");
    } else {
      await fireEvent.click(screen.getByRole("button", { name: "View original" }));
      await screen.findByText("Original failed");
      if (outcome === "failure") reject(new Error("Fit failed"));
      else
        finish(
          outcome === "terminal" ? { state: "failed" } : { state: "ready", generation_id: "fit" },
        );
      await new Promise((resolve) => setTimeout(resolve, 0));
      await tick();
      expect(screen.getByRole("alert").textContent).toBe("Original failed");
      expect(screen.queryByRole("button", { name: "Retry preview" })).toBeNull();
    }
    await fireEvent.click(screen.getByRole("button", { name: "View original" }));
    await screen.findByText("Original");
    expect(screen.queryByRole("alert")).toBeNull();
  },
);

it("loads the new photo's large preview after an obsolete original finishes decoding", async () => {
  const { props, preview } = setup();
  let finish!: () => void;
  vi.spyOn(props.photos, "original").mockResolvedValue("blob:original");
  vi.spyOn(HTMLImageElement.prototype, "decode").mockImplementation(
    function (this: HTMLImageElement) {
      return this.src === "blob:original"
        ? new Promise<void>((resolve) => (finish = resolve))
        : Promise.resolve();
    },
  );
  const view = render(PhotoLoupe, { ...props, items: [] });
  await screen.findByText("Fit preview");
  await fireEvent.click(screen.getByRole("button", { name: "View original" }));
  await waitFor(() => expect(finish).toBeTypeOf("function"));
  await view.rerender({ ...props, photo: props.items[5]!, items: [] });
  await screen.findByText("Fit preview");
  finish();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
  expect(screen.queryByText("Original")).toBeNull();
  expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:original");
  await fireEvent.doubleClick(view.container.querySelector(".photo-viewport")!);
  await screen.findByText("Large preview");
  expect(preview).toHaveBeenCalledWith(props.items[5], "large", expect.any(AbortSignal));
});

it.each([
  "Retry navigation",
  "Retry preview",
  "Reload photo",
  "Close photo viewer",
  "View original",
  "Metadata",
  "Next photo",
  "Show Photo 4.jpg",
])("closes with Escape focused on %s while respecting a nested dialog", async (buttonName) => {
  const { props, preview } = setup();
  if (buttonName === "Retry navigation") props.photos.error = "Navigation failed";
  else if (buttonName === "Retry preview" || buttonName === "Reload photo")
    preview.mockRejectedValueOnce(
      buttonName === "Reload photo"
        ? new APIError("Photo changed", 409, "photo_display_changed")
        : new Error("Preview failed"),
    );
  render(PhotoLoupe, { ...props, items: props.items });
  const button = (await screen.findAllByRole("button", { name: buttonName })).at(-1)!;
  button.focus();
  expect(document.activeElement).toBe(button);
  const nested = document.createElement("div");
  nested.setAttribute("role", "dialog");
  nested.setAttribute("aria-modal", "true");
  screen.getByRole("dialog").append(nested);
  await fireEvent.keyDown(button, { key: "Escape" });
  expect(props.onclose).not.toHaveBeenCalled();
  nested.remove();
  await fireEvent.keyDown(button, { key: "Escape" });
  expect(props.onclose).toHaveBeenCalledOnce();
});

function cacheSwapFixture() {
  const { props } = setup();
  const cache = fromStore(writable(props.cache));
  const currentPhoto = fromStore(writable<ReturnType<typeof photo> | undefined>(props.photo));
  const error = fromStore(writable(""));
  const view = render(PhotoLoupe, {
    ...props,
    items: [],
    get photo() {
      return currentPhoto.current;
    },
    get error() {
      return error.current;
    },
    get cache() {
      return cache.current;
    },
  });
  return { props, cache, currentPhoto, error, view };
}

it("retains Original through row and cache changes and releases unavailable or replaced decoded previews", async () => {
  const { props, cache, currentPhoto, error, view } = cacheSwapFixture();
  await screen.findByText("Fit preview");
  vi.spyOn(props.photos, "original").mockResolvedValue("blob:original");
  await fireEvent.click(screen.getByRole("button", { name: "View original" }));
  await screen.findByText("Original");
  const revised: ReturnType<typeof photo> = {
    ...props.photo,
    revision: props.photo.revision + 1,
    previews: { ...props.photo.previews, fit: { state: "ready", generation_id: "new-fit" } },
  };
  currentPhoto.current = revised;
  await tick();
  expect(screen.getByText("Original")).toBeTruthy();
  expect(URL.revokeObjectURL).not.toHaveBeenCalledWith("blob:original");
  cache.current = {
    get: vi.fn(async () => new Blob(["replacement-cache"])),
  } as unknown as PhotoPreviewCache;
  await tick();
  expect(screen.getByText("Original")).toBeTruthy();
  expect(URL.revokeObjectURL).not.toHaveBeenCalledWith("blob:original");
  currentPhoto.current = { ...revised, content_version_id: "fresh-version" };
  await screen.findByText("Fit preview");
  const displayed = () =>
    view.container.querySelector<HTMLImageElement>(".photo-viewport img")?.src;
  const revoked = vi.mocked(URL.revokeObjectURL);
  const releases: { displayed: string | undefined; revoked: string }[] = [];
  revoked.mockImplementation((url) => releases.push({ displayed: displayed(), revoked: url }));
  const pending = new Map<string, () => void>();
  vi.mocked(HTMLImageElement.prototype.decode).mockImplementation(
    function (this: HTMLImageElement) {
      const url = this.src;
      return new Promise<void>((resolve) => pending.set(url, resolve));
    },
  );
  const previous = displayed();
  const created = vi.mocked(URL.createObjectURL).mock.results.length;
  cache.current = {
    get: vi.fn(async () => new Blob(["replacement"])),
  } as unknown as PhotoPreviewCache;
  await waitFor(() => expect(pending.size).toBe(2));
  const [grid, fit] = [...pending.keys()];
  expect(revoked).not.toHaveBeenCalledWith(grid);
  expect(revoked).not.toHaveBeenCalledWith(fit);
  pending.get(fit!)!();
  await waitFor(() => expect(displayed()).toBe(fit));
  await waitFor(() => expect(revoked).toHaveBeenCalledWith(previous));
  expect(revoked).not.toHaveBeenCalledWith(grid);
  pending.get(grid!)!();
  await waitFor(() => expect(revoked).toHaveBeenCalledWith(grid));
  expect(displayed()).toBe(fit);
  const owned = vi
    .mocked(URL.createObjectURL)
    .mock.results.map((result) => result.value)
    .filter((url) => !revoked.mock.calls.some(([released]) => released === url));
  expect(owned).toEqual([fit]);
  expect(vi.mocked(URL.createObjectURL).mock.results.length).toBe(created + 2);
  expect(releases.every((release) => release.displayed !== release.revoked)).toBe(true);
  const image = view.container.querySelector<HTMLImageElement>(".photo-viewport img")!;
  const url = image.src;
  currentPhoto.current = undefined;
  error.current = "Photo unavailable";
  await screen.findByText("Photo unavailable");
  expect(view.container.querySelector(".photo-viewport img")).toBeNull();
  expect(image.isConnected).toBe(false);
  expect(URL.revokeObjectURL).toHaveBeenCalledWith(url);
});

it.each([
  {
    stage: "lookup failure",
    behavior: "keeps the newest Large generation after an obsolete preparation rejects",
  },
  {
    stage: "terminal",
    behavior: "keeps the newest Large generation after an obsolete terminal result",
  },
  {
    stage: "cache success",
    behavior: "ignores obsolete cached bytes after the newest Large generation renders",
  },
])("$behavior", async ({ stage }) => {
  const { props, preview, get } = setup();
  const oldBlob = new Blob(["old"]),
    newBlob = new Blob(["new"]);
  vi.spyOn(URL, "createObjectURL").mockImplementation((blob) =>
    blob === oldBlob ? "blob:old" : blob === newBlob ? "blob:new" : "blob:grid",
  );
  let finish!: () => void;
  let first = true;
  preview.mockImplementation(async (_photo, requested) => {
    if (requested !== "large") return { state: "ready", generation_id: requested };
    if (!first) return { state: "ready", generation_id: "new" };
    first = false;
    if (stage === "lookup failure" || stage === "terminal")
      return new Promise((resolve, reject) => {
        finish = () =>
          stage === "terminal"
            ? resolve({ state: "failed" })
            : reject(new Error("Obsolete lookup failed"));
      });
    return { state: "ready", generation_id: "old" };
  });
  get.mockImplementation(async (_asset, generation) => {
    if (generation === "new") return newBlob;
    if (generation !== "old") return new Blob(["grid"]);
    if (stage === "cache success")
      return new Promise((resolve) => {
        finish = () => resolve(oldBlob);
      });
    return oldBlob;
  });
  const view = render(PhotoLoupe, {
    ...props,
    items: [],
    get photo() {
      return props.photos.items[4] as ReturnType<typeof photo>;
    },
  });
  await screen.findByText("Fit preview");
  await fireEvent.doubleClick(view.container.querySelector(".photo-viewport")!);
  await waitFor(() => expect(finish, stage).toBeTypeOf("function"));
  const fitCalls = preview.mock.calls.filter(([, requested]) => requested === "fit").length;
  const largeCalls = preview.mock.calls.filter(([, requested]) => requested === "large").length;
  props.photos.items = props.photos.items.map((item, index) =>
    index === 4
      ? {
          ...props.photo,
          previews: {
            ...props.photo.previews,
            fit: { state: "ready", generation_id: "event-fit" },
          },
        }
      : item,
  );
  await waitFor(() => {
    expect(
      preview.mock.calls.filter(([, requested]) => requested === "fit"),
      stage,
    ).toHaveLength(fitCalls + 1);
    expect(
      preview.mock.calls.filter(([, requested]) => requested === "large"),
      stage,
    ).toHaveLength(largeCalls + 1);
  });
  await waitFor(() =>
    expect(view.container.querySelector<HTMLImageElement>(".photo-viewport img")?.src, stage).toBe(
      "blob:new",
    ),
  );
  await screen.findByText("Large preview");
  const calls = preview.mock.calls.filter(([, requested]) => requested === "large").length;
  finish();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
  expect(view.container.querySelector<HTMLImageElement>(".photo-viewport img")?.src, stage).toBe(
    "blob:new",
  );
  expect(screen.queryByRole("alert"), stage).toBeNull();
  await fireEvent.doubleClick(view.container.querySelector(".photo-viewport")!);
  await fireEvent.doubleClick(view.container.querySelector(".photo-viewport")!);
  expect(
    preview.mock.calls.filter(([, requested]) => requested === "large"),
    stage,
  ).toHaveLength(calls);
  if (stage === "cache success")
    expect(
      vi.mocked(URL.createObjectURL).mock.calls.some(([blob]) => blob === oldBlob),
      stage,
    ).toBe(false);
  view.unmount();
});

it("starts details closed, opens with Metadata and closes with I", async () => {
  const { props } = setup();
  render(PhotoLoupe, props);
  expect(screen.queryByRole("complementary", { name: "Photo details" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Metadata" }));
  expect(screen.getByRole("complementary", { name: "Photo details" })).toBeTruthy();
  await fireEvent.keyDown(screen.getByRole("dialog"), { key: "i" });
  expect(screen.queryByRole("complementary", { name: "Photo details" })).toBeNull();
});

it("hides an unreadable capture time when the file modified time placed the photo", async () => {
  const { props } = setup();
  props.photo.capture_time = "0000-01-01";
  props.photo.file_modified_time = "2023-11-04T18:22:03Z";
  render(PhotoLoupe, props);
  await fireEvent.click(await screen.findByRole("button", { name: "Metadata" }));
  expect(screen.getByText("File modified")).toBeTruthy();
  expect(screen.getByText("2023-11-04 18:22 UTC")).toBeTruthy();
  expect(screen.queryByText("Captured")).toBeNull();
});

it("shows the capture time and no file modified time for a photo placed by EXIF", async () => {
  const { props } = setup();
  props.photo.capture_time = "2023-11-04T18:22:03";
  props.photo.file_modified_time = null;
  render(PhotoLoupe, props);
  await fireEvent.click(await screen.findByRole("button", { name: "Metadata" }));
  expect(screen.getByText("Captured")).toBeTruthy();
  expect(screen.getByText("2023-11-04T18:22:03")).toBeTruthy();
  expect(screen.queryByText("File modified")).toBeNull();
});
