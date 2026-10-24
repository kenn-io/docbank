import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PhotoCompare from "./PhotoCompare.svelte";
import { APIError } from "./api-transport.js";
import { Photos } from "./photos.svelte.js";
import { idFreeChange, photo, photoChange } from "./photo-test-fixtures.js";
import type { PhotoPreviewCache } from "./photoPreviewCache.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  delete (HTMLImageElement.prototype as { decode?: unknown }).decode;
  vi.unstubAllGlobals();
});

const names = (dialog: HTMLElement) =>
  within(dialog)
    .getAllByRole("heading")
    .map((heading) => heading.textContent);

it.each([false, true])(
  "shares closeup retry results with full frames (removed: %s)",
  async (removed) => {
    const { photos, dialog } = compare();
    await within(dialog).findByText("Photo 1.jpg");
    await fireEvent.click(within(dialog).getByRole("button", { name: "Manual closeup" }));
    const frame = within(dialog).getByRole("button", { name: "Position closeup on Photo 1.jpg" });
    await fireEvent.click(frame);
    const closeups = await within(dialog).findByLabelText("Matching closeups");
    vi.mocked(photos.preview).mockRejectedValue(
      new APIError("Photo replaced", 409, "photo_display_changed"),
    );
    const closeup = within(closeups).getByRole("region", { name: "Closeup of Photo 1.jpg" });
    const retry = await within(closeup).findByRole("button", { name: "Retry preview" });
    const lookup = vi.spyOn(photos, "lookup");
    if (removed) lookup.mockRejectedValue(new APIError("Photo removed", 404, "not_found"));
    else
      lookup.mockResolvedValue({ ...photo(1), name: "Replacement.jpg", content_version_id: "v2" });
    await fireEvent.click(retry);
    await waitFor(() =>
      expect(names(dialog)).toEqual(
        removed
          ? ["Photo 2.jpg", "Photo 2.jpg"]
          : ["Replacement.jpg", "Photo 2.jpg", "Replacement.jpg", "Photo 2.jpg"],
      ),
    );
    if (!removed) {
      await waitFor(() =>
        expect(
          vi.mocked(photos.preview).mock.calls.filter(([row]) => row.content_version_id === "v2")
            .length,
        ).toBeGreaterThanOrEqual(2),
      );
    }
    photos.dispose();
  },
);

function compare(
  rows = [photo(1), photo(2), photo(3)],
  ids = rows.slice(0, 2).map((row) => row.asset_id),
  cache = { get: vi.fn() } as unknown as PhotoPreviewCache,
) {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
  const photos = new Photos("session", vi.fn());
  photos.items = rows;
  photos.started = true;
  vi.spyOn(photos, "lookupMany").mockImplementation(async (ids) =>
    ids.flatMap((id) => rows.filter((row) => row.asset_id === id)),
  );
  vi.spyOn(photos, "preview").mockResolvedValue({ state: "unsupported" });
  const onclose = vi.fn();
  render(PhotoCompare, {
    ids,
    photos,
    cache,
    onclose,
  });
  return { photos, onclose, dialog: screen.getByRole("dialog", { name: "Compare photos" }) };
}

it.each([1, 2])("reports %s unavailable selections and retries them", async (missing) => {
  const rows = [photo(1), photo(2), photo(3)];
  const { photos, dialog } = compare(
    rows.slice(0, 3 - missing),
    rows.map((row) => row.asset_id),
  );
  await within(dialog).findByText(
    `${missing} selected ${missing === 1 ? "photo is" : "photos are"} unavailable.`,
  );
  expect(within(dialog).getByText(`Compare ${3 - missing} photos`)).toBeTruthy();
  expect(names(dialog)).toEqual(missing === 1 ? ["Photo 1.jpg", "Photo 2.jpg"] : ["Photo 1.jpg"]);
  vi.mocked(photos.lookupMany).mockResolvedValue(rows);
  await fireEvent.click(within(dialog).getByRole("button", { name: /^Retry$/ }));
  await within(dialog).findByText("Compare 3 photos");
  expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg", "Photo 3.jpg"]);
  expect(within(dialog).queryByText(/selected photos? (is|are) unavailable/)).toBeNull();
  photos.dispose();
});

it("offers directional retry after linked navigation fails and keeps the pair until it succeeds", async () => {
  const rows = [photo(1), photo(2), photo(3)];
  const { photos, dialog, onclose } = compare(rows);
  const move = vi
    .spyOn(photos, "moveGroup")
    .mockResolvedValueOnce({ status: "failed", error: "Page unavailable" })
    .mockResolvedValueOnce({ status: "moved", ids: [rows[1]!.asset_id, rows[2]!.asset_id] });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next" }));
  const retry = await within(dialog).findByRole("button", { name: "Retry next photo" });
  expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]);
  retry.focus();
  await fireEvent.click(retry);
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
  expect(move.mock.calls).toEqual([
    [[rows[0]!.asset_id, rows[1]!.asset_id], 1],
    [[rows[0]!.asset_id, rows[1]!.asset_id], 1],
  ]);
  await waitFor(() => expect(document.activeElement).toBe(dialog.querySelector(".photo-compare")));
  await fireEvent.keyDown(document.activeElement!, { key: "Escape" });
  expect(onclose).toHaveBeenCalledOnce();
  photos.dispose();
});

it("navigates with keyboard focus initially and after activating Clear closeup", async () => {
  const { photos, dialog, onclose } = compare();
  const shell = dialog.querySelector(".photo-compare")!;
  try {
    await waitFor(() => expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]));
    await fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
    await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
    await fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" });
    await waitFor(() => expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]));
    await fireEvent.click(within(dialog).getByRole("button", { name: "Manual closeup" }));
    const frame = within(dialog).getByRole("button", {
      name: "Position closeup on Photo 1.jpg",
    });
    await fireEvent.keyDown(frame, { key: "ArrowRight" });
    await fireEvent.keyDown(frame, { key: "Enter" });
    await fireEvent.click(frame);
    const closeups = await within(dialog).findByLabelText("Matching closeups");
    await waitFor(() => expect(within(closeups).getAllByRole("heading")).toHaveLength(2));
    await waitFor(() =>
      expect(document.activeElement).toBe(dialog.querySelector(".photo-compare")),
    );
    const button = within(dialog).getByRole("button", { name: "Clear closeup" });
    button.focus();
    expect(document.activeElement).toBe(button);
    await fireEvent.click(button);
    await waitFor(() => expect(within(dialog).queryByLabelText("Matching closeups")).toBeNull());
    await fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
    await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
    await fireEvent.click(within(dialog).getByRole("button", { name: "Manual closeup" }));
    const nextFrame = within(dialog).getByRole("button", {
      name: "Position closeup on Photo 2.jpg",
    });
    await fireEvent.keyDown(nextFrame, { key: "Escape" });
    await waitFor(() => expect(document.activeElement).toBe(shell));
    await fireEvent.keyDown(shell, { key: "Escape" });
    expect(onclose).toHaveBeenCalledOnce();
  } finally {
    photos.dispose();
  }
});

it("serializes Next activations until the destination lookup finishes", async () => {
  const rows = [photo(1), photo(2), photo(3), photo(4)];
  const { photos, dialog } = compare(rows);
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]));
  let resolve!: (rows: ReturnType<typeof photo>[]) => void;
  vi.mocked(photos.lookupMany).mockImplementationOnce(
    () => new Promise((done) => (resolve = done)),
  );
  const move = vi.spyOn(photos, "moveGroup");
  const next = within(dialog).getByRole("button", { name: "Next" }) as HTMLButtonElement;
  await fireEvent.click(next);
  await waitFor(() => expect(photos.lookupMany).toHaveBeenCalledTimes(2));
  expect(next.disabled).toBe(true);
  await fireEvent.click(next);
  await fireEvent.keyDown(dialog.querySelector(".photo-compare")!, { key: "ArrowRight" });
  expect(move).toHaveBeenCalledOnce();
  expect(photos.lookupMany).toHaveBeenCalledTimes(2);
  expect(vi.mocked(photos.lookupMany).mock.calls[1]![0]).toEqual([
    rows[1]!.asset_id,
    rows[2]!.asset_id,
  ]);
  resolve(rows.slice(1, 3));
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
  await waitFor(() => expect(next.disabled).toBe(false));
  photos.dispose();
});

it("preserves the closeup when the destination lookup fails", async () => {
  const { photos, dialog } = compare([photo(1), photo(2), photo(3), photo(4)]);
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Manual closeup" }));
  const frame = within(dialog).getByRole("button", { name: "Position closeup on Photo 1.jpg" });
  await fireEvent.keyDown(frame, { key: "Enter" });
  await fireEvent.click(frame);
  const closeups = await within(dialog).findByLabelText("Matching closeups");
  vi.mocked(photos.lookupMany).mockRejectedValueOnce(new Error("Lookup unavailable"));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next" }));
  await within(dialog).findByText("Lookup unavailable");
  expect(within(dialog).getByLabelText("Matching closeups")).toBe(closeups);
  expect(
    within(closeups)
      .getAllByRole("heading")
      .map((heading) => heading.textContent),
  ).toEqual(["Photo 1.jpg", "Photo 2.jpg"]);
  const retry = within(dialog).getByRole("button", { name: /^Retry$/ });
  retry.focus();
  await fireEvent.click(retry);
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
  expect(within(dialog).queryByLabelText("Matching closeups")).toBeNull();
  expect(within(dialog).queryByText("Lookup unavailable")).toBeNull();
  await waitFor(() => expect(document.activeElement).toBe(dialog.querySelector(".photo-compare")));
  await fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" });
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]));
  vi.mocked(photos.lookupMany).mockRejectedValueOnce(new Error("Lookup unavailable"));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next" }));
  await within(dialog).findByText("Lookup unavailable");
  expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]);
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next" }));
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
  photos.dispose();
});

it("reads the shown photos again after a change that names them or after ID-free churn", async () => {
  const rows = [photo(1), photo(2), photo(3)];
  const { photos, dialog } = compare(rows);
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]));
  const closeup = within(dialog).getByRole("button", { name: "Manual closeup" });
  closeup.focus();
  const lookup = vi.mocked(photos.lookupMany);
  await photos.changed(photoChange("photo-3"));
  expect(lookup).toHaveBeenCalledOnce();
  lookup.mockResolvedValueOnce([
    {
      ...rows[0]!,
      name: "Renamed.jpg",
      previews: { ...rows[0]!.previews, grid: { state: "ready", generation_id: "grid-1" } },
    },
    rows[1]!,
  ]);
  await photos.changed(photoChange("photo-1"));
  await waitFor(() => expect(names(dialog)).toEqual(["Renamed.jpg", "Photo 2.jpg"]));
  await waitFor(() =>
    expect(
      vi
        .mocked(photos.preview)
        .mock.calls.filter(([row]) => row.asset_id === "photo-1")
        .at(-1)?.[0].previews.grid,
    ).toEqual({ state: "ready", generation_id: "grid-1" }),
  );
  lookup.mockResolvedValueOnce([rows[0]!]);
  await photos.changed(idFreeChange);
  await within(dialog).findByText("1 selected photo is unavailable.");
  expect(names(dialog)).toEqual(["Photo 1.jpg"]);
  // Live rereads leave keyboard focus where it was.
  expect(document.activeElement).toBe(closeup);
  photos.dispose();
});

it("keeps large images at 1:1 after a live preview update", async () => {
  vi.spyOn(URL, "createObjectURL").mockImplementation(
    (blob) => `blob:${(blob as Blob & { id: string }).id}`,
  );
  vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
  HTMLImageElement.prototype.decode = vi.fn().mockResolvedValue(undefined);
  const ready = (generation: string) => ({ state: "ready" as const, generation_id: generation });
  const previews = (revision: number) => ({
    grid: ready("grid-1"),
    fit: ready(`fit-${revision}`),
    large: ready(`large-${revision}`),
  });
  const rows = [photo(1), photo(2)];
  for (const row of rows) row.previews = previews(1);
  const { photos, dialog } = compare(rows, ["photo-1", "photo-2"], {
    get: vi.fn(async (_asset: string, id: string) => Object.assign(new Blob(["photo"]), { id })),
  } as unknown as PhotoPreviewCache);
  vi.mocked(photos.preview).mockRestore();
  const src = () => dialog.querySelector(".frames img")?.getAttribute("src");
  await waitFor(() => expect(src()).toBe("blob:fit-1"));
  dialog.querySelector<HTMLElement>(".frames .photo-viewport")!.dataset.zoomed = "1";
  await waitFor(() => expect(src()).toBe("blob:large-1"));
  vi.mocked(photos.lookupMany).mockResolvedValueOnce([
    { ...rows[0]!, previews: previews(2) },
    rows[1]!,
  ]);
  await photos.changed(photoChange("photo-1"));
  await waitFor(() => expect(src()).toBe("blob:large-2"));
  photos.dispose();
});

it.each(["replacement", "failure"])(
  "revalidates a pending destination after a change, %s",
  async (mode) => {
    HTMLImageElement.prototype.decode = vi.fn().mockResolvedValue(undefined);
    vi.spyOn(URL, "createObjectURL").mockReturnValue("blob:cached-photo");
    vi.spyOn(URL, "revokeObjectURL").mockImplementation(() => {});
    const rows = [photo(1), photo(2), photo(3), photo(4)];
    for (const row of rows) row.previews.grid = { state: "ready", generation_id: "grid-1" };
    const { photos, dialog } = compare(rows, ["photo-1", "photo-2"], {
      get: vi.fn(async () => new Blob(["synthetic-photo"])),
    } as unknown as PhotoPreviewCache);
    await within(dialog).findByRole("img", { name: "Photo 1.jpg" });
    const survivor = within(dialog).getByRole("region", { name: "Photo 2.jpg" });
    await fireEvent.click(within(dialog).getByRole("button", { name: "1:1 (Z)" }));
    const zoom = photos.compareView;
    const lookup = vi.mocked(photos.lookupMany);
    let stale!: (rows: ReturnType<typeof photo>[]) => void;
    lookup.mockImplementationOnce(() => new Promise((done) => (stale = done)));
    await fireEvent.click(within(dialog).getByRole("button", { name: "Next" }));
    await waitFor(() => expect(lookup).toHaveBeenCalledTimes(2));
    if (mode === "replacement") {
      lookup.mockResolvedValueOnce([rows[1]!, { ...rows[2]!, name: "Replaced.jpg" }]);
      await photos.changed(photoChange("photo-3"));
      expect(lookup.mock.calls[2]![0]).toEqual(["photo-2", "photo-3"]);
      await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Replaced.jpg"]));
      stale(rows.slice(1, 3));
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(names(dialog)).toEqual(["Photo 2.jpg", "Replaced.jpg"]);
    } else {
      lookup.mockImplementation(async (ids) => {
        if (ids.includes("photo-3")) throw new APIError("Service unavailable", 503, "unavailable");
        return [rows[1]!];
      });
      await expect(photos.changed(photoChange("photo-1"))).rejects.toThrow("Service unavailable");
      await within(dialog).findByText("1 selected photo is unavailable.");
      expect(within(dialog).queryByRole("img", { name: "Photo 1.jpg" })).toBeNull();
      expect(within(dialog).getByRole("region", { name: "Photo 2.jpg" })).toBe(survivor);
      expect(photos.compareView).toEqual(zoom);
      stale(rows.slice(1, 3));
      await fireEvent.click(within(dialog).getByRole("button", { name: /^Retry$/ }));
      await within(dialog).findByText("Service unavailable");
      expect(lookup.mock.calls.at(-1)![0]).toEqual(["photo-2", "photo-3"]);
      expect(names(dialog)).toEqual(["Photo 2.jpg"]);
    }
    photos.dispose();
  },
);
