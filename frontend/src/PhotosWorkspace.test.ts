import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotosWorkspace from "./PhotosWorkspace.svelte";
import { Photos } from "./photos.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import { photo } from "./photo-test-fixtures.js";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); localStorage.clear(); });

it("keeps loaded photos visible on paging failure and selects with touch checkboxes", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(1000);
  vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(800);
  const fetcher = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ items: [photo(1), photo(2)], total: 3, next_cursor: "next" })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ detail: "Page unavailable" }), { status: 503 }))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items: [photo(3)], total: 3 })));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  const cache = new PhotoPreviewCache("scoped", vi.fn());
  let view = render(PhotosWorkspace, { photos, cache });
  await screen.findByText("Page unavailable");
  expect(screen.getByRole("button", { name: "Select Photo 1.jpg" })).toBeTruthy();
  await fireEvent.click(screen.getByRole("checkbox", { name: "Select photo Photo 1.jpg" }));
  expect(await screen.findByText("1 selected photo")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Select loaded photos" }));
  expect(screen.getByText("2 selected photos")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("button", { name: "Select Photo 3.jpg" });
  let finish!: (response: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => finish = resolve));
  photos.cursor = "manual-page";
  await fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
  expect(await screen.findByText("Loading photos…")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  view.unmount();
  expect(fetcher.mock.calls[3][1].signal.aborted).toBe(true);
  let finishFresh!: (response: Response) => void;
  fetcher.mockImplementationOnce(() => new Promise(resolve => finishFresh = resolve));
  view = render(PhotosWorkspace, { photos, cache });
  await screen.findByRole("button", { name: "Select Photo 3.jpg" });
  expect(screen.getByText("2 selected photos")).toBeTruthy();
  finish(new Response(JSON.stringify({ items: [photo(99)], total: 4 })));
  await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(5));
  expect(photos.loading).toBe(true);
  expect(screen.queryByRole("button", { name: "Select Photo 99.jpg" })).toBeNull();
  finishFresh(new Response(JSON.stringify({ items: [photo(4)], total: 4 })));
  await screen.findByRole("button", { name: "Select Photo 4.jpg" });
  expect(screen.getByText("2 selected photos")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
  await fireEvent.keyDown(window, { key: "Escape" });
  await waitFor(() => expect(screen.queryByText(/selected photos/)).toBeNull());
  photos.dispose();
  await cache.dispose();
});
