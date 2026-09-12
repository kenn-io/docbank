import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ExportDrawer from "./ExportDrawer.svelte";
beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); Reflect.deleteProperty(Element.prototype, "scrollIntoView"); });
const input = { label: "Selected documents", members: [{ node_id: 1, version_id: "11111111-1111-4111-8111-111111111111", sha256: "a".repeat(64), size: 12 }] };

it("shows exact source scope and original warning before any server work, with explicit role omission choices", async () => {
  const fetcher = vi.spyOn(globalThis, "fetch");
  render(ExportDrawer, { session: "s", input, open: true, onclose: vi.fn(), onauthfailure: vi.fn() });
  expect(await screen.findByText("Selected documents")).toBeTruthy();
  expect(screen.getByText(/Originals are not redacted or sanitized/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Preview export" }).hasAttribute("disabled")).toBe(false);
  expect(screen.queryByRole("button", { name: "Start reviewed export" })).toBeNull();
  await fireEvent.click(screen.getByRole("combobox", { name: /^Text export policy/ }));
  expect(screen.getByRole("option", { name: "Optional — allow unavailable" })).toBeTruthy();
  expect(fetcher).not.toHaveBeenCalled();
});

it("disables empty sources and fences work when closed", async () => {
  const close = vi.fn();
  render(ExportDrawer, { session: "s", input: { label: "Empty selection", members: [] }, open: true, onclose: close, onauthfailure: vi.fn() });
  expect((await screen.findByRole("button", { name: "Preview export" })).hasAttribute("disabled")).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Close export" }));
  expect(close).toHaveBeenCalledOnce();
});

it("offers body PDFs with explicit partial, attachment, duplicate and bounded packaging choices", async () => {
  render(ExportDrawer, { session: "s", input, open: true, onclose: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("combobox", { name: /^Email body PDF/ }));
  await fireEvent.click(screen.getByRole("option", { name: "Include retained body PDFs" }));
  expect(screen.getByRole("button", { name: "Find retained PDF recipes" })).toBeTruthy();
  expect(screen.getByRole("combobox", { name: /^Email attachment outputs/ })).toBeTruthy();
  expect(screen.getByRole("combobox", { name: /^Partial email export/ }).textContent).toContain("Fail if any output is unavailable");
  expect(screen.getByRole("combobox", { name: /^Duplicate outputs/ }).textContent).toContain("Preserve every occurrence");
  expect(screen.getByRole("button", { name: "Preview export" }).hasAttribute("disabled")).toBe(true);
  expect(screen.getByText(/Only qualified nested-email PDFs/)).toBeTruthy();
});

it("routes an expired browser session to the app authentication handler", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ detail: "Session ended", code: "unauthorized" }), { status: 401, headers: { "Content-Type": "application/problem+json" } }));
  const onauthfailure = vi.fn(), onclose = vi.fn();
  render(ExportDrawer, { session: "s", input, open: true, onclose, onauthfailure });
  await fireEvent.click(await screen.findByRole("button", { name: "Preview export" }));
  await waitFor(() => expect(onauthfailure).toHaveBeenCalledOnce());
  expect(onclose).toHaveBeenCalledOnce();
});
