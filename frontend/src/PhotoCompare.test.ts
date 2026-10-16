import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PhotoCompare from "./PhotoCompare.svelte";
import { Photos } from "./photos.svelte.js";
import { photo } from "./photo-test-fixtures.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

const names = (dialog: HTMLElement) =>
  within(dialog)
    .getAllByRole("heading")
    .map((heading) => heading.textContent);

function compare(
  rows = [photo(1), photo(2), photo(3)],
  ids = rows.slice(0, 2).map((row) => row.asset_id),
  cursor?: string,
) {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("session", vi.fn());
  photos.items = cursor ? rows.slice(0, 2) : rows;
  photos.cursor = cursor;
  photos.started = true;
  vi.spyOn(photos, "lookup").mockImplementation(
    async (id) => rows.find((row) => row.asset_id === id)!,
  );
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  const onclose = vi.fn();
  render(PhotoCompare, {
    ids,
    photos,
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
    onclose,
  });
  return { photos, onclose, dialog: screen.getByRole("dialog", { name: "Compare photos" }) };
}

it("loads the next page, or offers a retry that keeps the pair, when linked navigation reaches the loaded edge", async () => {
  const rows = [photo(1), photo(2), photo(3)];
  const fetcher = vi
    .fn()
    .mockResolvedValueOnce(Response.json({ detail: "Page unavailable" }, { status: 503 }))
    .mockResolvedValueOnce(Response.json({ items: [rows[2]], total: 3 }));
  vi.stubGlobal("fetch", fetcher);
  const { photos, dialog } = compare(rows, undefined, "next-page");
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next" }));
  const retry = await within(dialog).findByRole("button", { name: "Retry next photo" });
  expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]);
  await fireEvent.click(retry);
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
  photos.dispose();
});

it("creates a manual closeup from keyboard position and confirmation", async () => {
  const { photos, dialog, onclose } = compare([photo(1), photo(2), photo(3), photo(4)]);
  const shell = dialog.querySelector(".photo-compare")!;
  await fireEvent.click(within(dialog).getByRole("button", { name: "Manual closeup" }));
  const frame = within(dialog).getByRole("button", {
    name: "Position closeup on Photo 1.jpg",
  });
  await fireEvent.keyDown(frame, { key: "ArrowRight" });
  await fireEvent.keyDown(frame, { key: "Enter" });
  await fireEvent.click(frame);
  await waitFor(() => expect(document.activeElement).toBe(shell));
  const closeups = within(dialog).getByLabelText("Matching closeups");
  await waitFor(() => expect(within(closeups).getAllByRole("heading")).toHaveLength(2));
  await fireEvent.keyDown(shell, { key: "ArrowRight" });
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Manual closeup" }));
  const nextFrame = within(dialog).getByRole("button", {
    name: "Position closeup on Photo 2.jpg",
  });
  await fireEvent.keyDown(nextFrame, { key: "Escape" });
  await waitFor(() => expect(document.activeElement).toBe(shell));
  await fireEvent.keyDown(shell, { key: "Escape" });
  expect(onclose).toHaveBeenCalledOnce();
  photos.dispose();
});
