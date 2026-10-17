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
    cache: { get: vi.fn() } as unknown as PhotoPreviewCache,
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

it("retries the next visible pair after its lookup fails instead of skipping a photo", async () => {
  const { photos, dialog } = compare([photo(1), photo(2), photo(3), photo(4)]);
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]));
  vi.mocked(photos.lookupMany).mockRejectedValueOnce(new Error("Lookup unavailable"));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next" }));
  await within(dialog).findByText("Lookup unavailable");
  expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]);
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next" }));
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

it("navigates with keyboard focus after activating Clear closeup", async () => {
  const { photos, dialog } = compare();
  try {
    await waitFor(() => expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]));
    await fireEvent.click(within(dialog).getByRole("button", { name: "Manual closeup" }));
    const frame = within(dialog).getByRole("button", {
      name: "Position closeup on Photo 1.jpg",
    });
    await fireEvent.keyDown(frame, { key: "Enter" });
    await fireEvent.click(frame);
    await within(dialog).findByLabelText("Matching closeups");
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
  } finally {
    photos.dispose();
  }
});

it("navigates with the initial keyboard focus", async () => {
  const { photos, dialog } = compare();
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]));
  await fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" });
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
  photos.dispose();
});

it("keeps the last panes after a lookup failure and retries the selected group", async () => {
  const rows = [photo(1), photo(2), photo(3)];
  const { photos, dialog } = compare(rows);
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]));
  vi.mocked(photos.lookupMany).mockRejectedValueOnce(new Error("Lookup unavailable"));
  await fireEvent.click(within(dialog).getByRole("button", { name: "Next" }));
  await within(dialog).findByText("Lookup unavailable");
  expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]);
  const retry = within(dialog).getByRole("button", { name: /^Retry$/ });
  retry.focus();
  await fireEvent.click(retry);
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
  expect(within(dialog).queryByText("Lookup unavailable")).toBeNull();
  await waitFor(() => expect(document.activeElement).toBe(dialog.querySelector(".photo-compare")));
  await fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" });
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 1.jpg", "Photo 2.jpg"]));
  photos.dispose();
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
  await fireEvent.click(within(dialog).getByRole("button", { name: /^Retry$/ }));
  await waitFor(() => expect(names(dialog)).toEqual(["Photo 2.jpg", "Photo 3.jpg"]));
  expect(within(dialog).queryByLabelText("Matching closeups")).toBeNull();
  photos.dispose();
});
