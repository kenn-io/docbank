import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import SelectionDock from "./SelectionDock.svelte";

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
  vi.restoreAllMocks();
});

it("describes a truncated page-local selection and exposes local controls", async () => {
  const onclear = vi.fn();
  const onselectvisible = vi.fn();
  render(SelectionDock, {
    selectedCount: 2,
    visibleDocumentCount: 4,
    truncated: true,
    onclear,
    onselectvisible,
    oncsv: vi.fn(),
  });

  const dock = screen.getByRole("region", { name: "Selected documents" });
  expect(dock.textContent).toContain("2 selected on this page");
  expect(dock.textContent).toContain("More results exist beyond this page");
  expect(dock.textContent?.toLowerCase()).not.toContain("all results");

  const selectVisible = screen.getByRole("button", {
    name: "Select visible documents",
  });
  selectVisible.focus();
  expect(document.activeElement).toBe(selectVisible);
  await fireEvent.click(selectVisible);
  expect(onselectvisible).toHaveBeenCalledOnce();

  await fireEvent.click(screen.getByRole("button", { name: "Clear selection" }));
  expect(onclear).toHaveBeenCalledOnce();
});

it("uses the dock close control to clear selection", async () => {
  const onclear = vi.fn();
  render(SelectionDock, {
    selectedCount: 1,
    visibleDocumentCount: 1,
    truncated: false,
    onclear,
    onselectvisible: vi.fn(),
    oncsv: vi.fn(),
  });

  expect(screen.queryByText("More results exist beyond this page")).toBeNull();
  await fireEvent.click(
    screen.getByRole("button", { name: "Clear selected documents" }),
  );
  expect(onclear).toHaveBeenCalledOnce();
});

it("labels and invokes the visible-page CSV action for the current selection", async () => {
  const oncsv = vi.fn();
  render(SelectionDock, {
    selectedCount: 2,
    visibleDocumentCount: 2,
    truncated: false,
    onclear: vi.fn(),
    onselectvisible: vi.fn(),
    oncsv,
  });

  await fireEvent.click(
    screen.getByRole("button", { name: "Export page CSV" }),
  );
  expect(oncsv).toHaveBeenCalledOnce();
});
