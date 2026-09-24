import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import BatesExportDrawer from "./BatesExportDrawer.svelte";
import { batesRecipeSHA256, type BatesRecipe } from "./bates";

// The daemon derives the reservation digest from the posted recipe.
async function recipeDigest(body: Record<string, unknown> | undefined): Promise<string> {
  return batesRecipeSHA256(body?.recipe as BatesRecipe);
}

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
  sessionStorage.clear();
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
    if (url === "/api/v1/bates/allocations") return json({ allocation_id: "33333333-3333-4333-8333-333333333333", namespace_id: namespace.namespace_id, snapshot_id: snapshotID, recipe_sha256: await recipeDigest(body), state: "reserved", start_sequence: 41, end_sequence: 42, labels: [
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
    start_at: 0,
  });

  await fireEvent.click(screen.getByRole("button", { name: "Reserve ACME000041–ACME000042" }));
  await screen.findByText("Range reserved");
  const reservation = requests.find((request) => request.url.endsWith("/allocations"))?.body;
  expect(reservation).toMatchObject({
    snapshot_id: snapshotID,
    recipe: { namespace_id: namespace.namespace_id, start_at: 41, restamp: false },
  });
  expect(reservation?.operation_id).toMatch(/^[0-9a-f-]{36}$/);
  expect(screen.getByRole("button", { name: "Start Bates export" })).toBeTruthy();
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

const allocationID = "33333333-3333-4333-8333-333333333333";
const labels = [
  { ordinal: 1, occurrence_id: "a".repeat(32), source_page: 2, output_page: 1, label: "ACME000041" },
  { ordinal: 2, occurrence_id: "b".repeat(32), source_page: 4, output_page: 1, label: "ACME000042" },
];
const earlier = {
  artifact_id: "55555555-5555-4555-8555-555555555555", allocation_id: "66666666-6666-4666-8666-666666666666",
  blob_sha256: "d".repeat(64), size: 900, media_type: "application/pdf", page_count: 1,
  recipe_sha256: "e".repeat(64), manifest_sha256: "f".repeat(64), state: "verified", created_at: "2026-09-20T00:00:00Z",
  pages: [{ ordinal: 1, occurrence_id: "c".repeat(32), source_blob_sha256: "c".repeat(64), source_page: 1, output_page: 1, label: "ACME000001" }],
};

type Route = (body: Record<string, unknown> | undefined, init: RequestInit) => Response | undefined;

function fakeServer(routes: Record<string, Route> = {}): { url: string; method: string; body?: Record<string, unknown> }[] {
  const requests: { url: string; method: string; body?: Record<string, unknown> }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init = {}) => {
    const url = String(input);
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(String(init.body)) as Record<string, unknown> : undefined;
    requests.push({ url, method, body });
    const routed = routes[`${method} ${url}`]?.(body, init);
    if (routed) return routed;
    if (url.startsWith("/api/v1/packages?")) return json({ items: [{ package_id: "pkg", package_name: "Synthetic review", direction: "received", state: "complete", snapshot_id: snapshotID, page_count: 2 }], limit: 250 });
    if (url.startsWith("/api/v1/bates/namespaces")) return json({ items: [namespace], total: 1 });
    if (url === "/api/v1/bates/exports?limit=50") return json({ items: [earlier], total: 1 });
    if (url === "/api/v1/bates/preview") return json({ namespace, start_sequence: 41, end_sequence: 42, stamped_nothing: true, labels });
    if (url === "/api/v1/bates/allocations") {
      return json({ allocation_id: allocationID, namespace_id: namespace.namespace_id, snapshot_id: snapshotID, recipe_sha256: await recipeDigest(body), state: "reserved", start_sequence: 41, end_sequence: 42, labels, created_at: "2026-09-21T00:01:00Z" }, 201);
    }
    if (url === "/api/v1/bates/exports" && method === "POST") {
      return json({
        artifact_id: "44444444-4444-4444-8444-444444444444", allocation_id: allocationID, blob_sha256: "a".repeat(64), size: 1200,
        media_type: "application/pdf", page_count: 2, recipe_sha256: await recipeDigest(body),
        manifest_sha256: "b".repeat(64), state: "verified", created_at: "2026-09-21T00:02:00Z",
        pages: labels.map((label) => ({ ...label, source_blob_sha256: "c".repeat(64) })),
      }, 201);
    }
    throw new Error(`unexpected request ${method} ${url}`);
  });
  return requests;
}

async function previewAndReserve(): Promise<void> {
  await fireEvent.click(await screen.findByRole("button", { name: "Preview Bates labels" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Reserve ACME000041–ACME000042" }));
}

it("opens a history export without replacing the reserved workflow", async () => {
  fakeServer();
  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await previewAndReserve();
  await screen.findByText("Range reserved");

  await fireEvent.click(screen.getByRole("button", { name: `Open Bates export ${earlier.allocation_id}` }));

  expect(await screen.findByText("Earlier Bates export")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Start Bates export" })).toBeTruthy();
  expect(screen.getByText("This reviewed preview is the exact range reserved below. Nothing has been stamped yet.")).toBeTruthy();
  expect(screen.queryByText("Stamped PDF ready")).toBeNull();
});

it("retries a failed reservation with the same operation instead of a new preview", async () => {
  let attempts = 0;
  const requests = fakeServer({
    "POST /api/v1/bates/allocations": () => (++attempts === 1 ? json({ detail: "The ledger is busy." }, 503) : undefined),
  });
  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await previewAndReserve();

  expect((await screen.findByRole("alert")).textContent).toContain("The ledger is busy.");
  expect(screen.queryByRole("button", { name: /^Reserve / })).toBeNull();
  expect(screen.getByRole("button", { name: "Preview Bates labels" }).hasAttribute("disabled")).toBe(true);

  await fireEvent.click(screen.getByRole("button", { name: "Retry reserve" }));
  await screen.findByText("Range reserved");

  const reservations = requests.filter((request) => request.url === "/api/v1/bates/allocations");
  expect(reservations).toHaveLength(2);
  expect(reservations[1]?.body).toEqual(reservations[0]?.body);
});

it("resumes an unfinished reservation after the drawer reopens", async () => {
  const requests = fakeServer({
    "POST /api/v1/bates/exports": () => json({ detail: "Drawer closed." }, 503),
  });
  const first = render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await previewAndReserve();
  await fireEvent.click(await screen.findByRole("button", { name: "Start Bates export" }));
  await screen.findByText("Drawer closed.");
  first.unmount();
  vi.restoreAllMocks();

  const resumed = fakeServer();
  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Resume Bates export" }));
  await screen.findByText("Stamped PDF ready");

  const original = requests.find((request) => request.url === "/api/v1/bates/allocations")?.body;
  expect(resumed.find((request) => request.url === "/api/v1/bates/allocations")?.body).toEqual(original);
  expect(resumed.some((request) => request.url === "/api/v1/bates/preview")).toBe(false);

  cleanup();
  fakeServer();
  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await screen.findByRole("button", { name: "Preview Bates labels" });
  expect(screen.queryByRole("button", { name: "Resume Bates export" })).toBeNull();
});

it("shows why publication history failed and signs out on an expired session", async () => {
  fakeServer({ "GET /api/v1/bates/exports?limit=50": () => json({ detail: "History index is rebuilding." }, 503) });
  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  expect(await screen.findByText("Publication history is unavailable: History index is rebuilding.")).toBeTruthy();
  cleanup();
  vi.restoreAllMocks();

  fakeServer({ "GET /api/v1/bates/exports?limit=50": () => json({ detail: "Session expired." }, 401) });
  const onauthfailure = vi.fn();
  const onclose = vi.fn();
  render(BatesExportDrawer, { session: "session", onclose, onauthfailure });
  await waitFor(() => expect(onauthfailure).toHaveBeenCalledOnce());
  expect(onclose).toHaveBeenCalledOnce();
});
