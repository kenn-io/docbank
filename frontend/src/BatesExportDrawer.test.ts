import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
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
  sessionStorage.clear();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

function json(value: unknown, status = 200): Response {
  return new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

it("previews selected pages before explicitly reserving the reviewed range", async () => {
  const requests: { url: string; body?: Record<string, unknown> }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init = {}) => {
    const url = String(input);
    const body = init.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined;
    requests.push({ url, body });
    if (url.startsWith("/api/v1/packages?"))
      return json({
        items: [
          {
            package_id: "pkg",
            package_name: "Synthetic review",
            direction: "received",
            state: "complete",
            snapshot_id: snapshotID,
            page_count: 2,
          },
        ],
        limit: 250,
      });
    if (url.startsWith("/api/v1/bates/namespaces")) {
      if (init.method === "POST") throw new Error("unexpected namespace create");
      return json({ items: [namespace], total: 1 });
    }
    if (url === "/api/v1/bates/exports?limit=50") return json({ items: [], total: 0 });
    if (url === "/api/v1/bates/preview")
      return json({
        namespace,
        start_sequence: 41,
        end_sequence: 42,
        stamped_nothing: true,
        labels: [
          {
            ordinal: 1,
            occurrence_id: "a".repeat(32),
            source_page: 2,
            output_page: 1,
            label: "ACME000041",
          },
          {
            ordinal: 2,
            occurrence_id: "b".repeat(32),
            source_page: 4,
            output_page: 1,
            label: "ACME000042",
          },
        ],
      });
    if (url === "/api/v1/bates/allocations")
      return json(
        {
          allocation_id: "33333333-3333-4333-8333-333333333333",
          namespace_id: namespace.namespace_id,
          snapshot_id: snapshotID,
          recipe_sha256: await recipeDigest(body),
          state: "reserved",
          start_sequence: 41,
          end_sequence: 42,
          labels: [
            {
              ordinal: 1,
              occurrence_id: "a".repeat(32),
              source_page: 2,
              output_page: 1,
              label: "ACME000041",
            },
            {
              ordinal: 2,
              occurrence_id: "b".repeat(32),
              source_page: 4,
              output_page: 1,
              label: "ACME000042",
            },
          ],
          created_at: "2026-09-21T00:01:00Z",
        },
        201,
      );
    throw new Error(`unexpected request ${url}`);
  });

  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Preview Bates labels" }));

  const preview = await screen.findByRole("region", { name: "Tentative Bates labels" });
  expect(within(preview).getByText("ACME000041")).toBeTruthy();
  expect(within(preview).getAllByRole("row")[1]!.textContent).toContain("Source page 2");
  expect(within(preview).getByText("Not reserved")).toBeTruthy();
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
    const body = init.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined;
    requests.push({ url, body });
    if (url.startsWith("/api/v1/packages?"))
      return json({
        items: [
          {
            package_id: "pkg",
            package_name: "Synthetic review",
            direction: "received",
            state: "complete",
            snapshot_id: snapshotID,
            page_count: 2,
          },
        ],
        limit: 250,
      });
    if (url === "/api/v1/bates/namespaces" && init.method === "POST")
      return json(
        {
          ...namespace,
          namespace_id: "44444444-4444-4444-8444-444444444444",
          prefix: "CASE",
          padding: 8,
        },
        201,
      );
    if (url.startsWith("/api/v1/bates/namespaces")) return json({ items: [], total: 0 });
    if (url === "/api/v1/bates/exports?limit=50") return json({ items: [], total: 0 });
    throw new Error(`unexpected request ${url}`);
  });

  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await screen.findByRole("heading", { name: "New namespace" });
  await fireEvent.input(screen.getByRole("textbox", { name: "Bates prefix" }), {
    target: { value: "CASE" },
  });
  await fireEvent.click(screen.getByRole("combobox", { name: /^Bates padding/ }));
  await fireEvent.click(screen.getByRole("option", { name: "8 digits" }));
  await fireEvent.click(screen.getByRole("button", { name: "Create namespace" }));

  await waitFor(() =>
    expect(
      screen.getByRole("button", { name: "Preview Bates labels" }).hasAttribute("disabled"),
    ).toBe(false),
  );
  expect(
    requests.find((request) => request.url === "/api/v1/bates/namespaces" && request.body)?.body,
  ).toEqual({ prefix: "CASE", suffix: "", padding: 8 });
  expect(requests.some((request) => request.url.includes("/preview"))).toBe(false);
});

const allocationID = "33333333-3333-4333-8333-333333333333";
const labels = [
  {
    ordinal: 1,
    occurrence_id: "a".repeat(32),
    source_page: 2,
    output_page: 1,
    label: "ACME000041",
  },
  {
    ordinal: 2,
    occurrence_id: "b".repeat(32),
    source_page: 4,
    output_page: 1,
    label: "ACME000042",
  },
];
const earlier = {
  artifact_id: "55555555-5555-4555-8555-555555555555",
  allocation_id: "66666666-6666-4666-8666-666666666666",
  blob_sha256: "d".repeat(64),
  size: 900,
  media_type: "application/pdf",
  page_count: 1,
  recipe_sha256: "e".repeat(64),
  manifest_sha256: "f".repeat(64),
  state: "verified",
  created_at: "2026-09-20T00:00:00Z",
  pages: [
    {
      ordinal: 1,
      occurrence_id: "c".repeat(32),
      source_blob_sha256: "c".repeat(64),
      source_page: 1,
      output_page: 1,
      label: "ACME000001",
    },
  ],
};

type Route = (body: Record<string, unknown> | undefined, init: RequestInit) => Response | undefined;

function fakeServer(
  routes: Record<string, Route> = {},
): { url: string; method: string; body?: Record<string, unknown> }[] {
  const requests: { url: string; method: string; body?: Record<string, unknown> }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init = {}) => {
    const url = String(input);
    const method = init.method ?? "GET";
    const body = init.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined;
    requests.push({ url, method, body });
    const routed = routes[`${method} ${url}`]?.(body, init);
    if (routed) return routed;
    if (url.startsWith("/api/v1/packages?"))
      return json({
        items: [
          {
            package_id: "pkg",
            package_name: "Synthetic review",
            direction: "received",
            state: "complete",
            snapshot_id: snapshotID,
            page_count: 2,
          },
        ],
        limit: 250,
      });
    if (url.startsWith("/api/v1/bates/namespaces")) return json({ items: [namespace], total: 1 });
    if (url === "/api/v1/bates/exports?limit=50") return json({ items: [earlier], total: 1 });
    if (url === "/api/v1/bates/preview")
      return json({
        namespace,
        start_sequence: 41,
        end_sequence: 42,
        stamped_nothing: true,
        labels,
      });
    if (url === "/api/v1/bates/allocations") {
      return json(
        {
          allocation_id: allocationID,
          namespace_id: namespace.namespace_id,
          snapshot_id: snapshotID,
          recipe_sha256: await recipeDigest(body),
          state: "reserved",
          start_sequence: 41,
          end_sequence: 42,
          labels,
          created_at: "2026-09-21T00:01:00Z",
        },
        201,
      );
    }
    if (url === "/api/v1/bates/exports" && method === "POST") {
      return json(
        {
          artifact_id: "44444444-4444-4444-8444-444444444444",
          allocation_id: allocationID,
          blob_sha256: "a".repeat(64),
          size: 1200,
          media_type: "application/pdf",
          page_count: 2,
          recipe_sha256: await recipeDigest(body),
          manifest_sha256: "b".repeat(64),
          state: "verified",
          created_at: "2026-09-21T00:02:00Z",
          pages: labels.map((label) => ({ ...label, source_blob_sha256: "c".repeat(64) })),
        },
        201,
      );
    }
    throw new Error(`unexpected request ${method} ${url}`);
  });
  return requests;
}

async function previewAndReserve(): Promise<void> {
  await fireEvent.click(await screen.findByRole("button", { name: "Preview Bates labels" }));
  await fireEvent.click(
    await screen.findByRole("button", { name: "Reserve ACME000041–ACME000042" }),
  );
}

it("opens a history export without replacing the reserved workflow", async () => {
  fakeServer();
  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await previewAndReserve();
  await screen.findByText("Range reserved");

  await fireEvent.click(
    screen.getByRole("button", { name: `Open Bates export ${earlier.allocation_id}` }),
  );

  expect(await screen.findByText("Earlier Bates export")).toBeTruthy();
  expect(screen.getByRole("button", { name: "Start Bates export" })).toBeTruthy();
  expect(screen.getByText("Reserved below. Not stamped yet.")).toBeTruthy();
  expect(screen.queryByText("Stamped PDF ready")).toBeNull();
});

it("retries a failed reservation with the same operation instead of a new preview", async () => {
  let attempts = 0;
  const requests = fakeServer({
    "POST /api/v1/bates/allocations": () =>
      ++attempts === 1 ? json({ detail: "The ledger is busy." }, 503) : undefined,
  });
  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await previewAndReserve();

  expect((await screen.findByRole("alert")).textContent).toContain("The ledger is busy.");
  expect(screen.queryByRole("button", { name: /^Reserve / })).toBeNull();
  expect(screen.queryByRole("button", { name: "Preview Bates labels" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Change settings" })).toBeNull();

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
  const first = render(BatesExportDrawer, {
    session: "session",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
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
  expect(resumed.find((request) => request.url === "/api/v1/bates/allocations")?.body).toEqual(
    original,
  );
  expect(resumed.some((request) => request.url === "/api/v1/bates/preview")).toBe(false);

  cleanup();
  fakeServer();
  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  await screen.findByRole("button", { name: "Preview Bates labels" });
  expect(screen.queryByRole("button", { name: "Resume Bates export" })).toBeNull();
});

it("shows why publication history failed and signs out on an expired session", async () => {
  fakeServer({
    "GET /api/v1/bates/exports?limit=50": () =>
      json({ detail: "History index is rebuilding." }, 503),
  });
  render(BatesExportDrawer, { session: "session", onclose: vi.fn(), onauthfailure: vi.fn() });
  expect(
    await screen.findByText("Publication history is unavailable: History index is rebuilding."),
  ).toBeTruthy();
  cleanup();
  vi.restoreAllMocks();

  fakeServer({
    "GET /api/v1/bates/exports?limit=50": () => json({ detail: "Session expired." }, 401),
  });
  const onauthfailure = vi.fn();
  const onclose = vi.fn();
  render(BatesExportDrawer, { session: "session", onclose, onauthfailure });
  await waitFor(() => expect(onauthfailure).toHaveBeenCalledOnce());
  expect(onclose).toHaveBeenCalledOnce();
});

it("lists imported and exported production sets across every page without requiring PDF pages", async () => {
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = new URL(String(input), "http://docbank.test");
    if (url.pathname === "/api/v1/packages")
      return json({
        items: url.searchParams.has("after")
          ? [
              {
                package_id: "second",
                package_name: "Exported images",
                direction: "produced",
                state: "sealed",
                page_count: 0,
                member_count: 2,
              },
            ]
          : [
              {
                package_id: "first",
                package_name: "Imported load file",
                snapshot_id: "44444444-4444-4444-8444-444444444444",
                direction: "received",
                state: "partial",
                page_count: 0,
                member_count: 3,
              },
            ],
        next_after: url.searchParams.has("after") ? undefined : "first",
        limit: 250,
      });
    if (url.pathname.startsWith("/api/v1/packages/by-id/"))
      return json({ detail: "Browser sessions cannot read package members." }, 403);
    if (url.pathname.startsWith("/api/v1/bates/allocations/"))
      return json({ snapshot_id: snapshotID });
    if (url.pathname === "/api/v1/bates/exports")
      return json({
        items: url.searchParams.has("after") ? [earlier] : [],
        next_after: url.searchParams.has("after") ? undefined : "earlier",
        total: 1,
      });
    throw new Error(`Unexpected production request ${String(input)}`);
  });
  render(BatesExportDrawer, {
    mode: "productions",
    session: "session",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  expect(await screen.findByText("Imported load file")).toBeTruthy();
  expect(await screen.findByText("Exported images")).toBeTruthy();
  await waitFor(() => expect(screen.queryByText("Loading…")).toBeNull());
  const list = screen.getByRole("list", { name: "Productions" });
  expect(within(list).getAllByRole("listitem")).toHaveLength(3);
  expect(fetcher.mock.calls.map(([url]) => String(url))).not.toContainEqual(
    expect.stringContaining("/members"),
  );
  expect(within(list).getByText("Imported")).toBeTruthy();
  expect(within(list).getAllByText("Exported")).toHaveLength(2);
  expect(list.querySelectorAll("details[open]")).toHaveLength(0);
  await fireEvent.click(within(list).getByText("Bates export"));
  expect(await screen.findByRole("button", { name: "Download verified PDF" })).toBeTruthy();
  expect(fetcher.mock.calls.every(([, init]) => !init?.method || init.method === "GET")).toBe(true);
});

it("shows a package and its Bates export once with receipt actions inside the collapsed row", async () => {
  const receipt = {
    ...earlier,
    page_count: 2,
    pages: labels.map((label) => ({ ...label, source_blob_sha256: "c".repeat(64) })),
  };
  const requests: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    requests.push(url);
    if (url.startsWith("/api/v1/packages?"))
      return json({
        items: [
          {
            package_id: "produced-set",
            package_name: "Review production",
            snapshot_id: snapshotID,
            direction: "produced",
            state: "sealed",
            page_count: 2,
            member_count: 1,
            manifest_sha256: "1".repeat(64),
            created_at: "2026-09-21T00:00:00Z",
          },
        ],
        limit: 250,
      });
    if (url === "/api/v1/bates/exports?limit=50") return json({ items: [receipt], total: 1 });
    if (url === `/api/v1/bates/allocations/${receipt.allocation_id}`)
      return json({ snapshot_id: snapshotID });
    if (url === `/api/v1/bates/exports/${receipt.allocation_id}`) return json(receipt);
    if (url === `/api/v1/bates/exports/${receipt.allocation_id}/download`)
      return json({
        allocation_id: receipt.allocation_id,
        blob_sha256: receipt.blob_sha256,
        size: receipt.size,
        url: "/api/daemon/web-download/file?ticket=synthetic",
        name: "review.pdf",
      });
    throw new Error(`Unexpected production request ${url}`);
  });
  const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
  render(BatesExportDrawer, {
    mode: "productions",
    session: "session",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await waitFor(() => {
    const name = screen.getByText("Review production");
    expect(name.closest("summary")?.textContent).toContain("ACME000041–ACME000042");
  });
  const name = screen.getByText("Review production");
  const list = screen.getByRole("list", { name: "Productions" });
  const row = within(list).getByRole("listitem");
  expect(within(list).getAllByRole("listitem")).toHaveLength(1);
  expect(row.querySelector("summary")?.textContent).toContain("ACME000041–ACME000042");
  expect(row.querySelector("summary")?.textContent).toContain("2 pages");
  expect(row.querySelector("summary")?.textContent).toContain("Exported");
  expect(row.querySelector("details")?.open).toBe(false);

  await fireEvent.click(name);
  expect(row.querySelector("details")?.open).toBe(true);
  expect(within(row).getByText("1".repeat(64))).toBeTruthy();
  expect(within(row).getByText(receipt.blob_sha256)).toBeTruthy();
  await fireEvent.click(within(row).getByRole("button", { name: "Refresh receipt" }));
  await waitFor(() => expect(requests).toContain(`/api/v1/bates/exports/${receipt.allocation_id}`));
  await waitFor(() =>
    expect(
      within(row).getByRole("button", { name: "Download verified PDF" }).hasAttribute("disabled"),
    ).toBe(false),
  );
  await fireEvent.click(within(row).getByRole("button", { name: "Download verified PDF" }));
  await waitFor(() => expect(click).toHaveBeenCalledOnce());
  expect(requests).toContain(`/api/v1/bates/exports/${receipt.allocation_id}/download`);
  await fireEvent.click(name);
  expect(row.querySelector("details")?.open).toBe(false);
});

it("shows Productions while an allocation is pending and loads other allocations independently", async () => {
  let finishAllocation!: (response: Response) => void;
  const nextAllocationID = "77777777-7777-4777-8777-777777777777";
  const fetcher = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.startsWith("/api/v1/packages?"))
      return json({
        items: [
          {
            package_id: "produced-set",
            package_name: "Review production",
            snapshot_id: snapshotID,
            direction: "produced",
            state: "sealed",
            page_count: 2,
          },
        ],
        limit: 250,
      });
    if (url === "/api/v1/bates/exports?limit=50")
      return json({ items: [earlier, { ...earlier, allocation_id: nextAllocationID }], total: 2 });
    if (url === `/api/v1/bates/allocations/${earlier.allocation_id}`)
      return new Promise<Response>((resolve) => {
        finishAllocation = resolve;
      });
    if (url === `/api/v1/bates/allocations/${nextAllocationID}`)
      return json({ snapshot_id: "88888888-8888-4888-8888-888888888888" });
    throw new Error(`Unexpected request ${url}`);
  });
  render(BatesExportDrawer, {
    mode: "productions",
    session: "session",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await waitFor(() => expect(finishAllocation).toBeTypeOf("function"));
  const list = await screen.findByRole("list", { name: "Productions" });
  expect(within(list).getByText("Review production")).toBeTruthy();
  expect(within(list).getAllByRole("listitem")).toHaveLength(3);
  await waitFor(() =>
    expect(fetcher.mock.calls.map(([url]) => String(url))).toContain(
      `/api/v1/bates/allocations/${nextAllocationID}`,
    ),
  );
  finishAllocation(json({ snapshot_id: snapshotID }));
  await waitFor(() => expect(within(list).getAllByRole("listitem")).toHaveLength(2));
  expect(within(list).getByText("Review production").closest("summary")?.textContent).toContain(
    "ACME000001–ACME000001",
  );
});

it.each(["packages", "allocation"])("keeps loaded exports when %s fails", async (failure) => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.startsWith("/api/v1/packages?"))
      return failure === "packages"
        ? json({ detail: "Package index unavailable." }, 503)
        : json({ items: [], limit: 250 });
    if (url === "/api/v1/bates/exports?limit=50") return json({ items: [earlier], total: 1 });
    if (url.startsWith("/api/v1/bates/allocations/"))
      return failure === "allocation"
        ? json({ detail: "Allocation unavailable." }, 503)
        : json({ snapshot_id: snapshotID });
    throw new Error(`Unexpected request ${url}`);
  });
  render(BatesExportDrawer, {
    mode: "productions",
    session: "session",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  expect(await screen.findByText("Bates export")).toBeTruthy();
  expect(
    await screen.findByText(
      failure === "packages" ? "Package index unavailable." : /Allocation unavailable/,
    ),
  ).toBeTruthy();
});
