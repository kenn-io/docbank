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

it("loads the next page when linked compare navigation reaches the loaded edge", async () => {
  const rows = [photo(1), photo(2), photo(3)];
  const fetcher = vi.fn().mockResolvedValue(Response.json({ items: [rows[2]], total: 3 }));
  vi.stubGlobal("fetch", fetcher);
  const { photos, dialog } = compare(rows, undefined, "next-page");
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next" }));
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
  expect(fetcher).toHaveBeenCalledOnce();
  expect(JSON.parse(fetcher.mock.calls[0]![1].body).cursor).toBe("next-page");
  photos.dispose();
});

it("retries a failed page when linked compare navigation reaches the loaded edge", async () => {
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

it.each([-1, 1] as const)(
  "replaces an expired cursor and retries direction %s atomically",
  async (direction) => {
    const rows = [photo(1), photo(2), photo(3), photo(4)];
    const cursors: (string | undefined)[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_url: string, init: RequestInit) => {
        const body = JSON.parse(String(init.body));
        if (body.facets?.length) return Response.json({ items: [], total: 4, facets: [] });
        cursors.push(body.cursor);
        if (body.cursor === "expired")
          return Response.json(
            { code: "cursor_expired", detail: "Cursor expired" },
            { status: 410 },
          );
        if (!body.cursor)
          return Response.json({ items: [rows[0]], total: 4, next_cursor: "replacement-2" });
        if (body.cursor === "replacement-2")
          return Response.json({ items: [rows[1]], total: 4, next_cursor: "replacement-3" });
        if (body.cursor === "replacement-3")
          return Response.json({ items: [rows[2]], total: 4, next_cursor: "replacement-4" });
        return Response.json({ items: [rows[3]], total: 4 });
      }),
    );
    const { photos, dialog } = compare(
      rows,
      rows.slice(1, 3).map((row) => row.asset_id),
      "expired",
    );
    await fireEvent.click(
      within(dialog).getByRole("button", { name: direction > 0 ? "Next" : "Previous" }),
    );
    const retry = await within(dialog).findByRole("button", {
      name: direction > 0 ? "Retry next photo" : "Retry previous photo",
    });
    expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]);
    await fireEvent.click(retry);
    await waitFor(() =>
      expect(names(dialog)).toEqual(
        direction > 0 ? ["Photo 3.jpg", "Photo 4.jpg"] : ["Photo 1.jpg", "Photo 2.jpg"],
      ),
    );
    expect(cursors).toEqual(
      direction > 0
        ? ["expired", undefined, "replacement-2", "replacement-3", "replacement-4"]
        : ["expired", undefined, "replacement-2", "replacement-3"],
    );
    photos.dispose();
  },
);

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
