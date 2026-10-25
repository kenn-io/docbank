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

it("keeps reports available without downloadable documents and routes each tool", async () => {
  const ondownload = vi.fn();
  const onreport = vi.fn();
  const onbates = vi.fn();
  const view = render(ExportMenu, { downloadDisabled: true, ondownload, onreport, onbates });
  await fireEvent.click(screen.getByRole("button", { name: "Export" }));
  await fireEvent.click(screen.getByRole("menuitem", { name: "Download files…" }));
  expect(ondownload).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("menuitem", { name: "Search term report…" }));
  expect(onreport).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: "Export" }));
  await fireEvent.click(screen.getByRole("menuitem", { name: "Bates-numbered PDFs…" }));
  expect(onbates).toHaveBeenCalledOnce();
  await view.rerender({ downloadDisabled: false });
  await fireEvent.click(screen.getByRole("button", { name: "Export" }));
  await fireEvent.click(screen.getByRole("menuitem", { name: "Download files…" }));
  expect(ondownload).toHaveBeenCalledOnce();
});
