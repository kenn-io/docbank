import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import BatesExportDrawer from "./BatesExportDrawer.svelte";

const namespace = {
  namespace_id: "11111111-1111-4111-8111-111111111111",
  prefix: "ACME",
  suffix: "",
  padding: 6,
  created_at: "2026-09-21T00:00:00Z",
};
const snapshotID = "22222222-2222-4222-8222-222222222222";

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } });
}

it("previews selected pages before explicitly reserving the reviewed range", async () => {
  const requests: { url: string; body?: Record<string, unknown> }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init = {}) => {
    const url = String(input);
    const body = init.body ? JSON.parse(String(init.body)) as Record<string, unknown> : undefined;
    requests.push({ url, body });
    if (url.startsWith("/api/v1/packages?")) return json({ items: [{ package_id: "pkg", package_name: "Synthetic review", direction: "received", state: "complete", snapshot_id: snapshotID, page_count: 2 }], limit: 250 });
    if (url.startsWith("/api/v1/bates/namespaces")) {
      if (init.method === "POST") throw new Error("unexpected namespace create");
      return json({ items: [namespace], total: 1 });
    }
    if (url === "/api/v1/bates/exports?limit=50") return json({ items: [], total: 0 });
    if (url === "/api/v1/bates/preview") return json({ namespace, start_sequence: 41, end_sequence: 42, stamped_nothing: true, labels: [
      { ordinal: 1, occurrence_id: "a".repeat(32), source_page: 2, output_page: 1, label: "ACME000041" },
      { ordinal: 2, occurrence_id: "b".repeat(32), source_page: 4, output_page: 1, label: "ACME000042" },
    ] });
    if (url === "/api/v1/bates/allocations") return json({ allocation_id: "33333333-3333-4333-8333-333333333333", namespace_id: namespace.namespace_id, snapshot_id: snapshotID, recipe_sha256: body?.recipe_sha256, state: "reserved", start_sequence: 41, end_sequence: 42, labels: [
      { ordinal: 1, occurrence_id: "a".repeat(32), source_page: 2, output_page: 1, label: "ACME000041" },
      { ordinal: 2, occurrence_id: "b".repeat(32), source_page: 4, output_page: 1, label: "ACME000042" },
    ], created_at: "2026-09-21T00:01:00Z" }, 201);
    throw new Error(`unexpected request ${url}`);
  });

  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Preview Bates labels" }));

  const preview = await screen.findByRole("region", { name: "Tentative Bates labels" });
  expect(within(preview).getByText("ACME000041")).toBeTruthy();
  expect(within(preview).getByText("Source page 2")).toBeTruthy();
  expect(screen.getByText("Nothing has been stamped or reserved.")).toBeTruthy();
  expect(requests.find((request) => request.url.endsWith("/preview"))?.body).toMatchObject({
    namespace_id: namespace.namespace_id,
    snapshot_id: snapshotID,
    recipe_sha256: "",
    start_at: 0,
  });

  await fireEvent.click(screen.getByRole("button", { name: "Reserve ACME000041–ACME000042" }));
  await screen.findByText("Range reserved");
  const reservation = requests.find((request) => request.url.endsWith("/allocations"))?.body;
  expect(reservation).toMatchObject({
    namespace_id: namespace.namespace_id,
    snapshot_id: snapshotID,
    start_at: 41,
  });
  expect(reservation?.recipe_sha256).toMatch(/^[0-9a-f]{64}$/);

  await new Promise((resolve) => setTimeout(resolve, 20));
  expect(requests.filter((request) => request.url.includes("/allocations/"))).toHaveLength(0);
});

it("creates and selects a namespace without reserving a range", async () => {
  const requests: { url: string; body?: Record<string, unknown> }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init = {}) => {
    const url = String(input);
    const body = init.body ? JSON.parse(String(init.body)) as Record<string, unknown> : undefined;
    requests.push({ url, body });
    if (url.startsWith("/api/v1/packages?")) return json({ items: [{ package_id: "pkg", package_name: "Synthetic review", direction: "received", state: "complete", snapshot_id: snapshotID, page_count: 2 }], limit: 250 });
    if (url === "/api/v1/bates/namespaces" && init.method === "POST") return json({ ...namespace, namespace_id: "44444444-4444-4444-8444-444444444444", prefix: "CASE", padding: 8 }, 201);
    if (url.startsWith("/api/v1/bates/namespaces")) return json({ items: [], total: 0 });
    if (url === "/api/v1/bates/exports?limit=50") return json({ items: [], total: 0 });
    throw new Error(`unexpected request ${url}`);
  });

  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await screen.findByText(/Create the first namespace/);
  await fireEvent.input(screen.getByRole("textbox", { name: "Bates prefix" }), { target: { value: "CASE" } });
  await fireEvent.click(screen.getByRole("combobox", { name: /^Bates padding/ }));
  await fireEvent.click(screen.getByRole("option", { name: "8 digits" }));
  await fireEvent.click(screen.getByRole("button", { name: "Create namespace" }));

  await waitFor(() => expect(screen.getByRole("button", { name: "Preview Bates labels" }).hasAttribute("disabled")).toBe(false));
  expect(requests.find((request) => request.url === "/api/v1/bates/namespaces" && request.body)?.body).toEqual({ prefix: "CASE", suffix: "", padding: 8 });
  expect(requests.some((request) => request.url.includes("/preview"))).toBe(false);
});
