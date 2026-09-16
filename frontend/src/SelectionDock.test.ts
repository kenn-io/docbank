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

it("offers the bounded tag action when supplied", async () => {
  const ontags = vi.fn();
  const { rerender } = render(SelectionDock, {
    selectedCount: 2,
    visibleDocumentCount: 3,
    truncated: false,
    onclear: vi.fn(),
    onselectvisible: vi.fn(),
    ontags,
    tagsDisabled: true,
    oncsv: vi.fn(),
  });

  expect((screen.getByRole("button", { name: "Edit tags" }) as HTMLButtonElement).disabled).toBe(true);
  await rerender({ tagsDisabled: false });
  await fireEvent.click(screen.getByRole("button", { name: "Edit tags" }));
  expect(ontags).toHaveBeenCalledOnce();
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

it("names snapshot-page tagging separately from the whole frozen query", async () => {
  const ontags = vi.fn();
  const onwholequerytags = vi.fn();
  render(SelectionDock, { props: {
    selectedCount: 1,
    visibleDocumentCount: 2,
    truncated: true,
    context: "snapshot",
    wholeQueryCount: 101,
    onclear: vi.fn(),
    onselectvisible: vi.fn(),
    ontags,
    onwholequerytags,
  } });

  expect(screen.getByText("1 selected on this frozen page")).toBeTruthy();
  expect(screen.getByText("Visible selection only · whole query has 101 documents")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Tag visible selection" }));
  expect(ontags).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: "Tag whole query" }));
  expect(onwholequerytags).toHaveBeenCalledOnce();
});

it("disables both snapshot tagging actions together", () => {
  render(SelectionDock, { props: { selectedCount: 1, visibleDocumentCount: 2, truncated: true,
    context: "snapshot", wholeQueryCount: 101, tagsDisabled: true,
    onclear: vi.fn(), onselectvisible: vi.fn(), ontags: vi.fn(), onwholequerytags: vi.fn() } });
  expect((screen.getByRole("button", { name: "Tag visible selection" }) as HTMLButtonElement).disabled).toBe(true);
  expect((screen.getByRole("button", { name: "Tag whole query" }) as HTMLButtonElement).disabled).toBe(true);
});
