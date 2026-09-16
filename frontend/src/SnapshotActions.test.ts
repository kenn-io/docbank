import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import SnapshotActions from "./SnapshotActions.svelte";
import { ACTION_MAX_BYTES } from "./actionRecovery.js";

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
    onabandon: vi.fn(),
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
    onabandon: vi.fn(),
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

it("discards a recovery file whose read finishes after the dialog closes", async () => {
  const onimport = vi.fn();
  const view = render(SnapshotActions, { selectedCount: 0, total: 1, catalog, catalogTotal: 1,
    disabled: false, onstart: vi.fn(), onimport, onresume: vi.fn(), onabandon: vi.fn(),
    onclose: () => view.unmount() });
  let release!: (bytes: ArrayBuffer) => void;
  const reading = new Promise<ArrayBuffer>((resolve) => { release = resolve; });
  const file = new File(["{}"], "action.json", { type: "application/json" });
  const read = vi.spyOn(file, "arrayBuffer").mockReturnValue(reading);
  await fireEvent.change(screen.getByLabelText("Import action recovery file"), { target: { files: [file] } });
  expect(read).toHaveBeenCalledOnce();
  await fireEvent.click(screen.getByRole("button", { name: "Close" }));
  release(new ArrayBuffer(0));
  await reading;
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(onimport).not.toHaveBeenCalled();
});

it("rejects an oversized recovery file before reading it", async () => {
  const onimport = vi.fn();
  render(SnapshotActions, { selectedCount: 0, total: 1, catalog, catalogTotal: 1,
    disabled: false, onstart: vi.fn(), onimport, onresume: vi.fn(), onabandon: vi.fn(), onclose: vi.fn() });
  const file = new File(["{}"], "oversized-action.json", { type: "application/json" });
  Object.defineProperty(file, "size", { value: ACTION_MAX_BYTES + 1 });
  const read = vi.spyOn(file, "arrayBuffer");
  await fireEvent.change(screen.getByLabelText("Import action recovery file"), { target: { files: [file] } });
  expect(read).not.toHaveBeenCalled();
  expect(onimport).not.toHaveBeenCalled();
  expect(screen.getByRole("alert").textContent).toContain("128 MiB");
});
