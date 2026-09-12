import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import ShortcutHelpModal from "./ShortcutHelpModal.svelte";

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  vi.restoreAllMocks();
});

const tax = {
  id: "33333333-3333-4333-8333-333333333333",
  name: "tax",
  revision: 2,
  assignment_count: 4,
};
const missing = "44444444-4444-4444-8444-444444444444";

it("makes browsing shortcuts discoverable and edits bounded tag bindings", async () => {
  const onbindingchange = vi.fn();
  render(ShortcutHelpModal, {
    vaultReady: true,
    catalog: [tax],
    catalogTotal: 2,
    bindings: { "1": tax.id, "2": missing },
    onbindingchange,
    onclose: vi.fn(),
  });

  expect(screen.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeTruthy();
  expect(screen.getByText("Focus search")).toBeTruthy();
  expect(screen.getByText("Next loaded row")).toBeTruthy();
  expect(screen.getByText("Toggle inspected file selection")).toBeTruthy();
  expect(
    screen.getByText(/Showing the first 1 of 2 tag definitions\./),
  ).toBeTruthy();
  expect(
    screen.getByRole("combobox", { name: `Tag shortcut 2: Not in loaded catalog · ${missing}` }),
  ).toBeTruthy();

  const slotOne = screen.getByRole("combobox", { name: "Tag shortcut 1: tax" });
  await fireEvent.click(slotOne);
  await fireEvent.click(screen.getByRole("option", { name: "Unassigned" }));
  expect(onbindingchange).toHaveBeenCalledWith("1", "");

  const slotThree = screen.getByRole("combobox", {
    name: "Tag shortcut 3: Unassigned",
  });
  await fireEvent.click(slotThree);
  await fireEvent.click(screen.getByRole("option", { name: "tax" }));
  expect(onbindingchange).toHaveBeenCalledWith("3", tax.id);
});

it("explains why tag bindings are disabled without a trustworthy vault ID", () => {
  render(ShortcutHelpModal, {
    vaultReady: false,
    catalog: [tax],
    catalogTotal: 1,
    bindings: {},
    onbindingchange: vi.fn(),
    onclose: vi.fn(),
  });

  expect(
    screen.getByText(
      "Tag shortcuts are unavailable until this browser session confirms the vault identity.",
    ),
  ).toBeTruthy();
  expect(
    screen
      .getByRole("combobox", { name: "Tag shortcut 1: Unassigned" })
      .hasAttribute("disabled"),
  ).toBe(true);
});
