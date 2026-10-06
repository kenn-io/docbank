import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/svelte";
import PhotoCell from "./PhotoCell.svelte";
import { photo } from "./photo-test-fixtures.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it("revokes mounted URLs and ignores previews that complete after unmount", async () => {
  Object.defineProperty(URL, "createObjectURL", { configurable: true, value: vi.fn(() => "blob:synthetic") });
  Object.defineProperty(URL, "revokeObjectURL", { configurable: true, value: vi.fn() });
  const item = photo(1);
  item.previews.grid = { state: "ready", generation_id: "generation" };
  let finish!: (blob: Blob) => void;
  const get = vi.fn().mockResolvedValueOnce(new Blob(["synthetic-jpeg"])).mockImplementationOnce(() => new Promise(resolve => finish = resolve));
  const props = { photo: item, cache: { get } as unknown as PhotoPreviewCache, selected: false, onclick: vi.fn(), oncheck: vi.fn() };
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
