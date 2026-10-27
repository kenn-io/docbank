import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import Toasts from "./Toasts.svelte";
import { clearToasts, getToasts, showToast } from "./toast.svelte.js";

afterEach(() => {
  cleanup();
  clearToasts();
  vi.useRealTimers();
});

it("runs a toast's action once and closes it", async () => {
  const undo = vi.fn();
  render(Toasts);
  showToast("Moved “a” to trash", { action: { label: "Undo", run: undo } });

  await fireEvent.click(await screen.findByRole("button", { name: "Undo" }));

  expect(undo).toHaveBeenCalledOnce();
  expect(screen.queryByText("Moved “a” to trash")).toBeNull();
});

it("expires on its own, announces errors as alerts, and keeps three at most", async () => {
  vi.useFakeTimers();
  render(Toasts);
  showToast("one");
  vi.advanceTimersByTime(8001);
  expect(getToasts()).toHaveLength(0);

  showToast("failed", { tone: "danger" });
  expect((await screen.findByRole("alert")).textContent).toContain("failed");
  for (const name of ["a", "b", "c"]) showToast(name);
  expect(getToasts().map((toast) => toast.message)).toEqual(["a", "b", "c"]);
});

it.each([
  "keyboard focus alone",
  "pointer leaves while a control has focus",
  "focus moves within the card and hover outlasts focus",
])("keeps a toast open while %s", async (interaction) => {
  vi.useFakeTimers();
  render(Toasts);
  showToast("Moved “a” to trash", { action: { label: "Undo", run: vi.fn() } });
  const undo = await screen.findByRole("button", { name: "Undo" });
  const dismiss = screen.getByRole("button", { name: "Dismiss" });
  const card = screen.getByRole("status");

  if (interaction === "pointer leaves while a control has focus") {
    await fireEvent.mouseEnter(card);
    undo.focus();
    await fireEvent.mouseLeave(card);
  } else {
    undo.focus();
  }
  if (interaction === "focus moves within the card and hover outlasts focus") {
    dismiss.focus();
    await vi.advanceTimersByTimeAsync(4001);
    expect(document.activeElement).toBe(dismiss);
    expect(screen.queryByRole("button", { name: "Undo" })).toBe(undo);
    await fireEvent.mouseEnter(card);
    dismiss.blur();
  }

  await vi.advanceTimersByTimeAsync(20_000);
  expect(getToasts()).toHaveLength(1);
  expect(screen.queryByRole("button", { name: "Undo" })).toBe(undo);
  if (interaction === "focus moves within the card and hover outlasts focus") {
    await fireEvent.mouseLeave(card);
  } else {
    expect(document.activeElement).toBe(undo);
    undo.blur();
  }
  await vi.advanceTimersByTimeAsync(4001);
  expect(getToasts()).toHaveLength(0);
  expect(screen.queryByRole("button", { name: "Undo" })).toBeNull();
});
