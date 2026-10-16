import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoCell from "./PhotoCell.svelte";
import { photo, touchTap } from "./photo-test-fixtures.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

it("opens by accessible name and plain click with the button as the return target", async () => {
  const onopen = vi.fn();
  render(PhotoCell, {
    photo: photo(1),
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    selected: false,
    onclick: vi.fn(),
    onopen,
  });
  const button = screen.getByRole("button", { name: "Open Photo 1.jpg" });
  await fireEvent.click(button);
  expect(onopen).toHaveBeenCalledExactlyOnceWith(button);
});

it.each(
  [false, true].flatMap((selected) =>
    ["native", "detail-1", "click pointerType"].map((shape) => ({ selected, shape })),
  ),
)(
  "selects and opens a $shape touch double-tap once with selected=$selected even if dblclick follows",
  async ({ selected, shape }) => {
    const onclick = vi.fn(),
      oncheck = vi.fn(),
      onopen = vi.fn();
    render(PhotoCell, {
      photo: photo(1),
      cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
      selected,
      onclick,
      oncheck,
      onopen,
    });
    const button = screen.getByRole("button", { name: "Select Photo 1.jpg" });
    for (const [time, detail] of [
      [1000, 1],
      [1100, shape === "native" ? 2 : 1],
    ]) {
      if (shape === "click pointerType") {
        const event = new MouseEvent("click", { bubbles: true, button: 0, detail });
        Object.defineProperty(event, "timeStamp", { value: time });
        Object.defineProperty(event, "pointerType", { value: "touch" });
        await fireEvent(button, event);
      } else await touchTap(button, time, detail);
    }
    await fireEvent.doubleClick(button);
    expect(onclick).toHaveBeenCalledOnce();
    if (selected) expect(oncheck).not.toHaveBeenCalled();
    else expect(oncheck).toHaveBeenCalledExactlyOnceWith(true, false);
    expect(onopen).toHaveBeenCalledExactlyOnceWith(button);
  },
);

it.each([1, 2])(
  "toggles selection after a cancelled scroll even with click detail=%s and dblclick",
  async (detail) => {
    const onclick = vi.fn(),
      onopen = vi.fn();
    render(PhotoCell, {
      photo: photo(1),
      cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
      selected: false,
      onclick,
      onopen,
    });
    const button = screen.getByRole("button", { name: "Select Photo 1.jpg" });
    await touchTap(button, 1000, 1);
    await fireEvent.pointerDown(button, { pointerType: "touch" });
    await fireEvent.pointerCancel(button, { pointerType: "touch" });
    await touchTap(button, 1100, detail);
    await fireEvent.doubleClick(button);
    expect(onclick).toHaveBeenCalledTimes(2);
    expect(onopen).not.toHaveBeenCalled();
    await touchTap(button, 1500, 1);
    await touchTap(button, 1600, detail);
    await fireEvent.doubleClick(button);
    expect(onopen).toHaveBeenCalledExactlyOnceWith(button);
  },
);

it("keeps compact touch clicks and never opens the viewer", async () => {
  const onclick = vi.fn(),
    oncheck = vi.fn(),
    onopen = vi.fn();
  render(PhotoCell, {
    photo: photo(1),
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    selected: false,
    compact: true,
    onclick,
    oncheck,
    onopen,
  });
  const button = screen.getByRole("button", { name: "Show Photo 1.jpg" });
  for (const detail of [1, 2]) {
    await touchTap(button, 1000 + (detail - 1) * 100, detail, "touch");
  }
  await fireEvent.doubleClick(button);
  await fireEvent.keyDown(button, { key: "Enter" });
  expect(onclick).toHaveBeenCalledTimes(2);
  expect(oncheck).not.toHaveBeenCalled();
  expect(onopen).not.toHaveBeenCalled();
  expect(screen.queryByRole("button", { name: "Open Photo 1.jpg" })).toBeNull();
});

type Tap = (button: HTMLElement) => Promise<void>;
const pairRejections: { name: string; clicks: number; run: Tap }[] = [
  {
    name: "detail-1 clicks over 350 ms apart",
    clicks: 2,
    run: async (button) => {
      for (const time of [1000, 1351]) await touchTap(button, time);
    },
  },
  {
    name: "quick taps at opposite ends of a wide tile",
    clicks: 2,
    run: async (button) => {
      await touchTap(button, 1000, 1, undefined, { x: 5, y: 50 });
      await touchTap(button, 1100, 1, undefined, { x: 400, y: 50 });
    },
  },
  ...(["checkbox", "another tile"] as const).map((target) => ({
    name: `a ${target} click between images`,
    clicks: target === "checkbox" ? 2 : 3,
    run: async (button: HTMLElement) => {
      const intervening =
        target === "checkbox"
          ? screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" })
          : screen.getByRole("button", { name: "Select Photo 2.jpg" });
      await touchTap(button, 1000);
      await touchTap(intervening, 1100);
      await touchTap(button, 1200);
    },
  })),
  ...([1, 2] as const).map((detail) => ({
    name: `another tile's pointer cancel then a detail-${detail} tap and double-click`,
    clicks: 2,
    run: async (button: HTMLElement) => {
      const other = screen.getByRole("button", { name: "Select Photo 2.jpg" });
      await touchTap(button, 1000);
      await fireEvent.pointerDown(other, { pointerType: "touch" });
      await fireEvent.pointerCancel(other, { pointerType: "touch" });
      await touchTap(button, 1100, detail);
      await fireEvent.doubleClick(button);
    },
  })),
  ...[
    { pointerType: "mouse", detail: 1 },
    { pointerType: "touch", detail: 0 },
    { pointerType: "touch", detail: 1, ctrlKey: true },
    { pointerType: "touch", detail: 1, metaKey: true },
    { pointerType: "touch", detail: 1, shiftKey: true },
    { pointerType: "touch", detail: 1, altKey: true },
  ].map((options) => ({
    name: `a click with ${JSON.stringify(options)}`,
    clicks: 3,
    run: async (button: HTMLElement) => {
      await touchTap(button, 1000);
      await fireEvent.pointerDown(button, { pointerType: options.pointerType });
      const event = new MouseEvent("click", { bubbles: true, button: 0, ...options });
      Object.defineProperty(event, "timeStamp", { value: 1050 });
      await fireEvent(button, event);
      await touchTap(button, 1100);
    },
  })),
];

it.each(pairRejections)(
  "does not count two taps as a double-tap for $name",
  async ({ clicks, run }) => {
    const onclick = vi.fn(),
      onopen = vi.fn();
    const props = {
      cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
      selected: false,
      onopen,
    };
    render(PhotoCell, { ...props, photo: photo(1), onclick });
    render(PhotoCell, { ...props, photo: photo(2), onclick });
    await run(screen.getByRole("button", { name: "Select Photo 1.jpg" }));
    expect(onclick).toHaveBeenCalledTimes(clicks);
    expect(onopen).not.toHaveBeenCalled();
  },
);

function previewFixture(failure: string, compact = false) {
  let urls = 0;
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: vi.fn(() => `blob:synthetic-${++urls}`),
  });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  const item = photo(1);
  item.previews.grid = { state: "ready", generation_id: "generation" };
  const get = vi.fn(
    async (_id: string, _generation: string, _signal: AbortSignal, _reload: boolean) =>
      new Blob(["synthetic-jpeg"]),
  );
  const onclick = vi.fn(),
    onopen = vi.fn();
  if (failure === "cache") get.mockRejectedValueOnce(new Error("Preview unavailable"));
  let finish!: (blob: Blob) => void;
  if (failure === "lifecycle")
    get
      .mockImplementationOnce(() => new Promise((resolve) => (finish = resolve)))
      .mockResolvedValueOnce(new Blob(["replacement-jpeg"]))
      .mockImplementationOnce(() => new Promise((resolve) => (finish = resolve)));
  const props = {
    photo: item,
    cache: { get } as unknown as PhotoPreviewCache,
    selected: false,
    compact,
    onclick,
    oncheck: vi.fn(),
    onopen,
  };
  const first = render(PhotoCell, props);
  return {
    item,
    get,
    props,
    first,
    get finish() {
      return finish;
    },
    onclick,
    onopen,
  };
}

it("preserves pending previews through row replacement and ignores late completion after unmount", async () => {
  const fixture = previewFixture("lifecycle");
  const { get, first, item, props } = fixture;
  await waitFor(() => expect(get).toHaveBeenCalledOnce());
  const signal = get.mock.calls[0][2];
  await first.rerender({ photo: { ...item } });
  expect(signal.aborted).toBe(false);
  expect(get).toHaveBeenCalledOnce();
  fixture.finish(new Blob(["completed"]));
  const old = await screen.findByRole("img"),
    oldURL = (old as HTMLImageElement).src;
  const replacement = {
    ...item,
    previews: {
      ...item.previews,
      grid: { state: "ready" as const, generation_id: "replacement-grid" },
    },
  };
  await first.rerender({ ...props, photo: replacement });
  await waitFor(() =>
    expect((screen.getByRole("img") as HTMLImageElement).src).toBe("blob:synthetic-2"),
  );
  expect(get).toHaveBeenLastCalledWith(
    item.asset_id,
    "replacement-grid",
    expect.any(AbortSignal),
    false,
  );
  expect(URL.revokeObjectURL).toHaveBeenCalledWith(oldURL);
  first.unmount();
  expect(get.mock.calls[0][2].aborted).toBe(true);
  expect(get.mock.calls[1][2].aborted).toBe(true);
  expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:synthetic-2");
  const second = render(PhotoCell, props);
  await waitFor(() => expect(get).toHaveBeenCalledTimes(3));
  second.unmount();
  fixture.finish(new Blob(["late-jpeg"]));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(URL.createObjectURL).toHaveBeenCalledTimes(2);
});

it.each([
  { failure: "cache", compact: false },
  { failure: "cache", compact: true },
  { failure: "decode", compact: false },
])("retries a $failure preview failure, compact=$compact", async ({ failure, compact }) => {
  const { get, onclick, onopen } = previewFixture(failure, compact);
  if (failure === "decode") await fireEvent.error(await screen.findByRole("img"));
  const retry = await screen.findByRole("button", {
    name: compact ? "Retry preview for Photo 1.jpg" : "Retry preview",
  });
  retry.focus();
  await fireEvent.click(retry);
  await screen.findByRole("img");
  expect(get.mock.calls.map((call) => call[3])).toEqual([false, true]);
  expect(onclick).not.toHaveBeenCalled();
  expect(onopen).not.toHaveBeenCalled();
  if (compact)
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Show Photo 1.jpg" }));
});

it("measures only quarter turns and releases the observer when rotation returns to zero", async () => {
  const created = vi.fn(),
    observe = vi.fn(),
    disconnect = vi.fn();
  vi.stubGlobal(
    "ResizeObserver",
    class {
      constructor() {
        created();
      }
      observe = observe;
      disconnect = disconnect;
    },
  );
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(300);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(200);
  Object.defineProperty(URL, "createObjectURL", {
    configurable: true,
    value: vi.fn(() => "blob:rotated"),
  });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  const item = photo(1);
  item.previews.grid = { state: "ready", generation_id: "generation" };
  const props = {
    photo: item,
    cache: { get: vi.fn(async () => new Blob(["synthetic JPEG"])) } as unknown as PhotoPreviewCache,
    selected: false,
    onclick: vi.fn(),
  };
  const view = render(PhotoCell, props);
  const image = (await screen.findByRole("img")) as HTMLImageElement;
  expect(image.style.width).toBe("100%");
  expect(image.style.height).toBe("100%");
  expect(created).not.toHaveBeenCalled();
  await view.rerender({ ...props, photo: { ...item, rotation: 180 } });
  expect(created).not.toHaveBeenCalled();
  await view.rerender({ ...props, photo: { ...item, rotation: 90 } });
  await waitFor(() => expect(observe).toHaveBeenCalledOnce());
  expect(created).toHaveBeenCalledOnce();
  expect(observe).toHaveBeenCalledOnce();
  await view.rerender(props);
  expect(disconnect).toHaveBeenCalledOnce();
  expect(((await screen.findByRole("img")) as HTMLImageElement).style.width).toBe("100%");
});
