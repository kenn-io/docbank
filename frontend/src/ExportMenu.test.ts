import { beforeEach, afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import ExportMenu from "./ExportMenu.svelte";

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it("routes downloads and Bates exports", async () => {
  const ondownload = vi.fn();
  const onbates = vi.fn();
  const view = render(ExportMenu, { downloadDisabled: true, ondownload, onbates });
  await fireEvent.click(screen.getByRole("button", { name: "Export" }));
  await fireEvent.click(screen.getByRole("menuitem", { name: "Download files…" }));
  expect(ondownload).not.toHaveBeenCalled();
  expect(screen.queryByRole("menuitem", { name: "Search term report…" })).toBeNull();
  await fireEvent.click(screen.getByRole("menuitem", { name: "Bates-numbered PDFs…" }));
  expect(onbates).toHaveBeenCalledOnce();
  await view.rerender({ downloadDisabled: false });
  await fireEvent.click(screen.getByRole("button", { name: "Export" }));
  await fireEvent.click(screen.getByRole("menuitem", { name: "Download files…" }));
  expect(ondownload).toHaveBeenCalledOnce();
});
