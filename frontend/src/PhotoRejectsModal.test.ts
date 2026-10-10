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
  photos.rejects = { digest: "a".repeat(64), photos: 2, files: 2, unchanged: 65, mixed: [], mixed_count: 0 };
  render(PhotoRejectsModal, { photos, onclose: vi.fn(), onmove: vi.fn() });
  expect(screen.getByText("Close this dialog and select up to 64 photos.")).toBeTruthy();
  expect(screen.getByText("2 photos · 2 files including sidecars")).toBeTruthy();
  expect((screen.getByRole("button", { name: "Move 2 to trash" }) as HTMLButtonElement).disabled).toBe(false);
  await fireEvent.click(screen.getByRole("combobox", { name: "Rejects scope: Library" }));
  expect((screen.getByRole("option", { name: "Selected photos (65)" }) as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole("option", { name: "Library" }) as HTMLButtonElement).disabled).toBe(false);
  photos.dispose();
});
