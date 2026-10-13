import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoCell from "./PhotoCell.svelte";
import { photo } from "./photo-test-fixtures.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it("keeps a hidden Open button for click-only assistive input", async () => {
  const onopen = vi.fn();
  render(PhotoCell, { photo: photo(1), cache: { get: vi.fn() } as unknown as PhotoPreviewCache, selected: false, onclick: vi.fn(), onopen });
  const button = screen.getByRole("button", { name: "Open Photo 1.jpg" });
  expect(button.classList.contains("kit-sr-only")).toBe(true);
  expect(button.getAttribute("tabindex")).toBe("-1");
  await fireEvent.click(button);
  expect(onopen).toHaveBeenCalledExactlyOnceWith(button);
});

it.each([false, true])("selects and opens a touch double-tap once with selected=%s", async selected => {
  const onclick = vi.fn(), oncheck = vi.fn(), onopen = vi.fn();
  render(PhotoCell, { photo: photo(1), cache: { get: vi.fn() } as unknown as PhotoPreviewCache, selected, onclick, oncheck, onopen });
  const button = screen.getByRole("button", { name: "Select Photo 1.jpg" });
  for (const detail of [1, 2]) {
    const event = new MouseEvent("click", { bubbles: true, detail });
    Object.defineProperty(event, "pointerType", { value: "touch" });
    await fireEvent(button, event);
  }
  await fireEvent.doubleClick(button);
  expect(onclick).toHaveBeenCalledOnce();
  if (selected) expect(oncheck).not.toHaveBeenCalled();
  else expect(oncheck).toHaveBeenCalledExactlyOnceWith(true, false);
  expect(onopen).toHaveBeenCalledExactlyOnceWith(button);
});

it("keeps compact touch clicks and never opens the viewer", async () => {
  const onclick = vi.fn(), oncheck = vi.fn(), onopen = vi.fn();
  render(PhotoCell, { photo: photo(1), cache: { get: vi.fn() } as unknown as PhotoPreviewCache, selected: false, compact: true, onclick, oncheck, onopen });
  const button = screen.getByRole("button", { name: "Show Photo 1.jpg" });
  for (const detail of [1, 2]) {
    const event = new MouseEvent("click", { bubbles: true, detail });
    Object.defineProperty(event, "pointerType", { value: "touch" });
    await fireEvent(button, event);
  }
  await fireEvent.doubleClick(button);
  await fireEvent.keyDown(button, { key: "Enter" });
  expect(onclick).toHaveBeenCalledTimes(2);
  expect(oncheck).not.toHaveBeenCalled();
  expect(onopen).not.toHaveBeenCalled();
  expect(screen.queryByRole("button", { name: "Open Photo 1.jpg" })).toBeNull();
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
