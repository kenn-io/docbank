import { render, fireEvent, screen, cleanup } from "@testing-library/svelte";
import { afterEach, expect, it, vi } from "vite-plus/test";
import RedactionDrawer from "./RedactionDrawer.svelte";
import type { SelectedSource } from "./selectedSource.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
const source: SelectedSource = {
  kind: "live",
  key: "synthetic",
  nodeID: 3,
  mutationRevision: 1,
  versionID: "00000000-0000-4000-8000-000000000001",
  blobHash: "a".repeat(64),
  size: 1083,
  name: "synthetic.pdf",
  path: "/synthetic.pdf",
  mimeType: "application/pdf",
  modifiedAt: "2026-01-01T00:00:00Z",
};
it("Escape dismisses only the inner discard dialog, then closing aborts preparation", async () => {
  let signal: AbortSignal | undefined;
  vi.spyOn(globalThis, "fetch").mockImplementation(
    (_url, init) =>
      new Promise((_resolve, reject) => {
        signal = init?.signal as AbortSignal;
        signal.addEventListener("abort", () => reject(signal?.reason), { once: true });
      }),
  );
  const onclose = vi.fn();
  render(RedactionDrawer, {
    session: "synthetic-tab",
    source,
    authorizationRevision: 1,
    onclose,
    onauthfailure: vi.fn(),
  });
  await screen.findByText("Preparing PDF…");
  await fireEvent.click(screen.getByRole("button", { name: "Close redaction" }));
  const inner = await screen.findByRole("dialog", { name: "Discard redaction?" });
  await fireEvent.keyDown(inner, { key: "Escape" });
  expect(screen.queryByRole("dialog", { name: "Discard redaction?" })).toBeNull();
  expect(screen.getByRole("dialog", { name: "Redact synthetic.pdf" })).toBeTruthy();
  expect(onclose).not.toHaveBeenCalled();
  expect(signal?.aborted).toBe(false);
  await fireEvent.click(screen.getByRole("button", { name: "Cancel preparation" }));
  expect(signal?.aborted).toBe(true);
  expect(onclose).toHaveBeenCalledOnce();
});

it("routes an expired session to the app and never presents an empty successful editor", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(JSON.stringify({ title: "Session expired", status: 401, code: "unauthorized" }), {
      status: 401,
      headers: { "Content-Type": "application/problem+json" },
    }),
  );
  const onauthfailure = vi.fn();
  render(RedactionDrawer, {
    session: "synthetic-tab",
    source,
    authorizationRevision: 1,
    onclose: vi.fn(),
    onauthfailure,
  });
  await vi.waitFor(() => expect(onauthfailure).toHaveBeenCalledOnce());
  expect(screen.queryByRole("button", { name: "Preview redaction" })).toBeNull();
  expect(screen.queryByText("No masks")).toBeNull();
});
