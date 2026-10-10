import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import PhotoRejectsModal from "./PhotoRejectsModal.svelte";
import { Photos } from "./photos.svelte.js";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); Reflect.deleteProperty(Element.prototype, "scrollIntoView"); });

it("keeps Library preview available when selected scope exceeds 64 photos", async () => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  const photos = new Photos("scoped", vi.fn());
  photos.selection.selectedIDs = new Set(Array.from({ length: 65 }, (_, i) => `photo-${i}`));
  photos.rejects = { targets: Array.from({ length: 1000 }, (_, i) => ({ asset_id: `photo-${i}`, revision: 1, member_revision: 1 })), photos: 1001, movable: 1000, files: 1001, unchanged: 65, mixed: [{ asset_id: "mixed", members: [{ file_id: "raw", name: "photo.raw", flag: "reject", in_trash: false }, { file_id: "jpeg", name: "photo.jpg", flag: "pick", in_trash: true }] }], mixed_count: 1 };
  render(PhotoRejectsModal, { photos, onclose: vi.fn(), onmove: vi.fn() });
  expect(screen.getByText("1,001 photos · 1,001 files including sidecars")).toBeTruthy();
  expect(screen.getByText("photo.raw: reject · photo.jpg: pick (in Trash)")).toBeTruthy();
  expect((screen.getByRole("button", { name: "Move 1000 to trash" }) as HTMLButtonElement).disabled).toBe(false);
  expect(screen.getByText("This moves 1,000 now. Choose Move rejects again for the remaining 1.")).toBeTruthy();
  await fireEvent.click(screen.getByRole("combobox", { name: "Rejects scope: Library" }));
  expect((screen.getByRole("option", { name: "Selected photos (65)" }) as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole("option", { name: "Library" }) as HTMLButtonElement).disabled).toBe(false);
  photos.dispose();
});
