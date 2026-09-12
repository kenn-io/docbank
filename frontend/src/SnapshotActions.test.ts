import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import SnapshotActions from "./SnapshotActions.svelte";

const catalog = [{
  id: "22222222-2222-4222-8222-222222222222",
  name: "Review",
  revision: 2,
  assignment_count: 12,
}];

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

async function chooseReview(): Promise<void> {
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("combobox", { name: /Tag for snapshot action/ }));
  await fireEvent.click(screen.getByRole("option", { name: "Review" }));
}

it("makes the visible selection and whole frozen query distinct actions", async () => {
  const onstart = vi.fn();
  render(SnapshotActions, {
    selectedCount: 2,
    total: 101,
    catalog,
    catalogTotal: 1,
    disabled: false,
    onstart,
    onimport: vi.fn(),
    onresume: vi.fn(),
    onclose: vi.fn(),
  });

  expect(screen.getByText(/2 documents selected on this visible page/)).toBeTruthy();
  expect(screen.getByText(/all 101 documents in the frozen query/)).toBeTruthy();
  await chooseReview();

  await fireEvent.click(screen.getByRole("button", { name: "Add tag to visible selection" }));
  expect(onstart).toHaveBeenLastCalledWith({
    scope: "selection",
    tagID: catalog[0].id,
    assign: true,
  });

  await fireEvent.click(screen.getByRole("button", { name: "Remove tag from whole query" }));
  expect(onstart).toHaveBeenLastCalledWith({
    scope: "query",
    tagID: catalog[0].id,
    assign: false,
  });
});

it("reads an imported recovery file without starting an action", async () => {
  const onstart = vi.fn();
  const onimport = vi.fn();
  const onresume = vi.fn();
  render(SnapshotActions, {
    selectedCount: 0,
    total: 101,
    catalog,
    catalogTotal: 1,
    disabled: false,
    onstart,
    onimport,
    onresume,
    onclose: vi.fn(),
  });

  const file = new File([new Uint8Array([1, 2, 3])], "action.docbank-action.json", {
    type: "application/json",
  });
  await fireEvent.change(screen.getByLabelText("Import action recovery file"), {
    target: { files: [file] },
  });

  expect(onimport).toHaveBeenCalledOnce();
  expect(Array.from(onimport.mock.calls[0][0] as Uint8Array)).toEqual([1, 2, 3]);
  expect(onstart).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "Resume retained action" }));
  expect(onresume).toHaveBeenCalledOnce();
});
