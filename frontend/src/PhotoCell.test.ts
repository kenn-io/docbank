import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoCell from "./PhotoCell.svelte";
import { photo, touchTap } from "./photo-test-fixtures.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it("opens by accessible name and plain click with the button as the return target", async () => {
  const onopen = vi.fn();
  render(PhotoCell, { photo: photo(1), cache: { get: vi.fn() } as unknown as PhotoPreviewCache, selected: false, onclick: vi.fn(), onopen });
  const button = screen.getByRole("button", { name: "Open Photo 1.jpg" });
  await fireEvent.click(button);
  expect(onopen).toHaveBeenCalledExactlyOnceWith(button);
});

it.each([false, true].flatMap(selected => ["native", "detail-1"].map(shape => ({ selected, shape }))))("selects and opens a $shape touch double-tap once with selected=$selected even if dblclick follows", async ({ selected, shape }) => {
  const onclick = vi.fn(), oncheck = vi.fn(), onopen = vi.fn();
  render(PhotoCell, { photo: photo(1), cache: { get: vi.fn() } as unknown as PhotoPreviewCache, selected, onclick, oncheck, onopen });
  const button = screen.getByRole("button", { name: "Select Photo 1.jpg" });
  for (const [time, detail] of [[1000, 1], [1100, shape === "native" ? 2 : 1]]) {
    await touchTap(button, time, detail);
  }
  await fireEvent.doubleClick(button);
  expect(onclick).toHaveBeenCalledOnce();
  if (selected) expect(oncheck).not.toHaveBeenCalled();
  else expect(oncheck).toHaveBeenCalledExactlyOnceWith(true, false);
  expect(onopen).toHaveBeenCalledExactlyOnceWith(button);
});

it.each([1, 2])("toggles selection after a cancelled scroll even with click detail=%s and dblclick", async detail => {
  const onopen = vi.fn();
  const selectedIDs = new Set(["a"]);
  const onclick = vi.fn(() => { if (selectedIDs.has("b")) selectedIDs.delete("b"); else selectedIDs.add("b"); });
  const props = { cache: { get: vi.fn() } as unknown as PhotoPreviewCache, onopen };
  render(PhotoCell, { ...props, photo: photo(1), selected: true, onclick: vi.fn() });
  const view = render(PhotoCell, { ...props, photo: photo(2), selected: false, onclick });
  const button = screen.getByRole("button", { name: "Select Photo 2.jpg" });
  const tap = async (time: number, detail: number) => {
    await touchTap(button, time, detail);
    await view.rerender({ selected: selectedIDs.has("b") });
  };
  await tap(1000, 1);
  expect(button.getAttribute("aria-pressed")).toBe("true");
  await fireEvent.pointerDown(button, { pointerType: "touch" });
  await fireEvent.pointerCancel(button, { pointerType: "touch" });
  await tap(1100, detail);
  await fireEvent.doubleClick(button);
  expect(onclick).toHaveBeenCalledTimes(2);
  expect(selectedIDs).toEqual(new Set(["a"]));
  expect(button.getAttribute("aria-pressed")).toBe("false");
  expect(onopen).not.toHaveBeenCalled();
  await tap(1500, 1);
  await tap(1600, detail);
  await fireEvent.doubleClick(button);
  expect(onopen).toHaveBeenCalledExactlyOnceWith(button);
});

it.each(["checkbox", "another tile"])("breaks a detail-1 pair when %s is clicked between images", async target => {
  const onclick = vi.fn(), onopen = vi.fn();
  const props = { cache: { get: vi.fn() } as unknown as PhotoPreviewCache, selected: false, onclick, onopen };
  render(PhotoCell, { ...props, photo: photo(1) });
  if (target === "another tile") render(PhotoCell, { ...props, photo: photo(2) });
  const button = screen.getByRole("button", { name: "Select Photo 1.jpg" });
  await fireEvent.pointerDown(button, { pointerType: "touch" });
  await fireEvent.click(button, { detail: 1 });
  const intervening = target === "checkbox" ? screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" }) : screen.getByRole("button", { name: "Select Photo 2.jpg" });
  await fireEvent.pointerDown(intervening, { pointerType: "touch" });
  await fireEvent.click(intervening, { detail: 1 });
  await fireEvent.pointerDown(button, { pointerType: "touch" });
  await fireEvent.click(button, { detail: 1 });
  expect(onclick).toHaveBeenCalledTimes(target === "checkbox" ? 2 : 3);
  expect(onopen).not.toHaveBeenCalled();
});

it("keeps detail-1 clicks over 350 ms apart as single clicks", async () => {
  const onclick = vi.fn(), onopen = vi.fn();
  render(PhotoCell, { photo: photo(1), cache: { get: vi.fn() } as unknown as PhotoPreviewCache, selected: false, onclick, onopen });
  const button = screen.getByRole("button", { name: "Select Photo 1.jpg" });
  for (const time of [1000, 1351]) {
    await touchTap(button, time);
  }
  expect(onclick).toHaveBeenCalledTimes(2);
  expect(onopen).not.toHaveBeenCalled();
});

it("keeps compact touch clicks and never opens the viewer", async () => {
  const onclick = vi.fn(), oncheck = vi.fn(), onopen = vi.fn();
  render(PhotoCell, { photo: photo(1), cache: { get: vi.fn() } as unknown as PhotoPreviewCache, selected: false, compact: true, onclick, oncheck, onopen });
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

it.each([
  { pointerType: "mouse", detail: 1 }, { pointerType: "touch", detail: 0 },
  { pointerType: "touch", detail: 1, ctrlKey: true }, { pointerType: "touch", detail: 1, metaKey: true },
  { pointerType: "touch", detail: 1, shiftKey: true }, { pointerType: "touch", detail: 1, altKey: true },
])("clears a pending touch pair for clicks with %j", async options => {
  const onclick = vi.fn(), onopen = vi.fn();
  render(PhotoCell, { photo: photo(1), cache: { get: vi.fn() } as unknown as PhotoPreviewCache, selected: false, onclick, onopen });
  const button = screen.getByRole("button", { name: "Select Photo 1.jpg" });
  await fireEvent.pointerDown(button, { pointerType: "touch" });
  await fireEvent.click(button, { detail: 1 });
  await fireEvent.pointerDown(button, { pointerType: options.pointerType });
  await fireEvent.click(button, options);
  await fireEvent.pointerDown(button, { pointerType: "touch" });
  await fireEvent.click(button, { detail: 1 });
  expect(onclick).toHaveBeenCalledTimes(3);
  expect(onopen).not.toHaveBeenCalled();
});

it("revokes mounted URLs and ignores previews that complete after unmount", async () => {
  Object.defineProperty(URL, "createObjectURL", { configurable: true, value: vi.fn(() => "blob:synthetic") });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  const item = photo(1);
  item.previews.grid = { state: "ready", generation_id: "generation" };
  let finish!: (blob: Blob) => void;
  const get = vi.fn().mockResolvedValueOnce(new Blob(["synthetic-jpeg"])).mockImplementationOnce(() => new Promise(resolve => finish = resolve));
  const props = { photo: item, cache: { get } as unknown as PhotoPreviewCache, selected: false, onclick: vi.fn(), oncheck: vi.fn(), onopen: vi.fn() };
  const first = render(PhotoCell, props);
  await screen.findByRole("img");
  first.unmount();
  expect(get.mock.calls[0][2].aborted).toBe(true);
  expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:synthetic");
  const second = render(PhotoCell, props);
  await waitFor(() => expect(get).toHaveBeenCalledTimes(2));
  second.unmount();
  expect(get.mock.calls[1][2].aborted).toBe(true);
  finish(new Blob(["late-jpeg"]));
  await new Promise(resolve => setTimeout(resolve, 0));
  expect(URL.createObjectURL).toHaveBeenCalledTimes(1);
});

it("reloads a preview from the network after the image fails to decode", async () => {
  Object.defineProperty(URL, "createObjectURL", { configurable: true, value: vi.fn(() => "blob:synthetic") });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  const item = photo(1);
  item.previews.grid = { state: "ready", generation_id: "generation" };
  const get = vi.fn(async (_id: string, _generation: string, _signal: AbortSignal, _reload: boolean) => new Blob(["synthetic-jpeg"]));
  render(PhotoCell, { photo: item, cache: { get } as unknown as PhotoPreviewCache, selected: false, onclick: vi.fn(), oncheck: vi.fn(), onopen: vi.fn() });
  await fireEvent.error(await screen.findByRole("img"));
  await fireEvent.click(screen.getByRole("button", { name: "Retry preview" }));
  await screen.findByRole("img");
  expect(get.mock.calls.map(call => call[3])).toEqual([false, true]);
});
