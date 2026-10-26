import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
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
  const { rerender } = render(SelectionDock, {
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
  await rerender({ selectedCount: 4 });
  expect((selectVisible as HTMLButtonElement).disabled).toBe(true);
  await rerender({ selectedCount: 2 });
  expect((selectVisible as HTMLButtonElement).disabled).toBe(false);

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
  await fireEvent.click(screen.getByRole("button", { name: "Clear selected documents" }));
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

  expect(
    (screen.getByRole("button", { name: "Tag selected…" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  await rerender({ tagsDisabled: false });
  await fireEvent.click(screen.getByRole("button", { name: "Tag selected…" }));
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

  await fireEvent.click(screen.getByRole("button", { name: "Export page CSV" }));
  expect(oncsv).toHaveBeenCalledOnce();
});

it.each(["live", "snapshot"] as const)(
  "offers the same document actions in the %s view and stays closed until something is selected",
  async (context) => {
    const props = {
      visibleDocumentCount: 2,
      truncated: false,
      context,
      onclear: vi.fn(),
      onselectvisible: vi.fn(),
      ontags: vi.fn(),
      onexport: vi.fn(),
      onreport: vi.fn(),
    };
    const view = render(SelectionDock, { props: { ...props, selectedCount: 0 } });
    expect(screen.queryByRole("button", { name: "Tag selected…" })).toBeNull();
    await view.rerender({ selectedCount: 1 });
    expect(
      screen.getByText(context === "snapshot" ? "1 selected" : "1 selected on this page"),
    ).toBeTruthy();
    expect(
      screen
        .getAllByRole("button")
        .map((button) => button.textContent?.trim())
        .filter(Boolean),
    ).toEqual([
      "Select visible documents",
      "Clear selection",
      "Tag selected…",
      "Report selected documents",
      "Export selection",
    ]);
  },
);
