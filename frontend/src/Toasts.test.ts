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

it("stays open while one of its controls has keyboard focus", async () => {
  vi.useFakeTimers();
  render(Toasts);
  showToast("Moved “a” to trash", { action: { label: "Undo", run: vi.fn() } });
  const undo = await screen.findByRole("button", { name: "Undo" });
  await fireEvent.focusIn(undo);
  vi.advanceTimersByTime(20_000);
  expect(getToasts()).toHaveLength(1);
  await fireEvent.focusOut(undo);
  vi.advanceTimersByTime(4001);
  expect(getToasts()).toHaveLength(0);
});
