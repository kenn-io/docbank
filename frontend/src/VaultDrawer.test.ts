import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import VaultDrawer from "./VaultDrawer.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function mockSystemRequests() {
  return vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url.includes("/storage"))
      return new Response(
        JSON.stringify({
          loose_blobs: 0,
          loose_bytes: 0,
          packs: 0,
          pack_stored_bytes: 0,
          packed_blobs: 0,
          packed_raw_bytes: 0,
          packed_stored_bytes: 0,
          dead_packed_bytes: 0,
          stores: [],
        }),
      );
    if (url.includes("/backup/"))
      return new Response(
        JSON.stringify({ repository: { id: "demo", path: "/demo/backups" }, items: [] }),
      );
    if (url.includes("/jobs")) return new Response(JSON.stringify({ items: [], lanes: [] }));
    throw new Error(`Unexpected request ${url}`);
  });
}

it("lists the sections in order and opens on disk use", async () => {
  mockSystemRequests();
  render(VaultDrawer, { session: "synthetic", onclose: vi.fn(), onauthfailure: vi.fn() });
  const drawer = screen.getByRole("dialog", { name: "System" });
  expect(
    within(drawer)
      .getAllByRole("tab")
      .map((tab) => tab.textContent?.trim()),
  ).toEqual([
    "Disk use",
    "Running now",
    "Backups",
    "Protected folders",
    "Usage reporting",
    "Search by meaning",
  ]);
  expect(within(drawer).getByRole("tab", { name: "Disk use", selected: true })).toBeTruthy();
  expect(await within(drawer).findByText("Total on disk")).toBeTruthy();
});

it("keeps navigation and close inside the active tool's dialog", async () => {
  mockSystemRequests();
  const onclose = vi.fn();
  render(VaultDrawer, { session: "synthetic", onclose, onauthfailure: vi.fn() });
  await fireEvent.click(screen.getByRole("tab", { name: "Backups" }));
  const drawer = screen.getByRole("dialog", { name: "System" });
  await fireEvent.click(within(drawer).getByRole("tab", { name: "Usage reporting" }));
  const privacy = screen.getByRole("dialog", { name: "System" });
  expect(within(privacy).getByRole("region", { name: "Usage reporting" })).toBeTruthy();
  await fireEvent.click(within(privacy).getByRole("tab", { name: "Running now" }));
  const activityTab = await screen.findByRole("tab", { name: "Running now", selected: true });
  const activity = screen.getByRole("dialog", { name: "System" });
  expect(activity.contains(activityTab)).toBe(true);
  expect(within(activity).getAllByRole("button", { name: /^Close/, hidden: true })).toHaveLength(1);
  expect(
    within(activity)
      .getAllByRole("heading")
      .map((heading) => heading.textContent),
  ).toEqual(["System"]);
  await fireEvent.click(within(activity).getByRole("tab", { name: "Usage reporting" }));
  expect(screen.getAllByRole("dialog")).toHaveLength(1);
  await fireEvent.click(screen.getByRole("button", { name: "Close System" }));
  expect(onclose).toHaveBeenCalledOnce();
});

const off = {
  active: false,
  provider: "",
  download_bytes: 290_000_000,
  installing: false,
  restart_required: false,
};
const plan = { fingerprint: "f".repeat(64), server: "local", flow: [] };

function mockSearchSetup(status: () => object, apply: () => Promise<Response>) {
  const requests: { url: string; init?: RequestInit }[] = [];
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    requests.push({ url, init });
    if (url.endsWith("/processing/local/status")) return new Response(JSON.stringify(status()));
    if (url.endsWith("/processing/local/plans")) return new Response(JSON.stringify(plan));
    if (url.endsWith("/processing/local/setup")) return apply();
    if (url.includes("/jobs")) return new Response(JSON.stringify({ items: [], lanes: [] }));
    throw new Error(`Unexpected request ${url}`);
  });
  return { fetch, requests };
}

function openSearchTab() {
  return render(VaultDrawer, {
    session: "synthetic",
    initialTab: "searchByMeaning",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
}

it("turns on search by meaning and keeps setup running after the drawer closes", async () => {
  let saved = false;
  let finish: (response: Response) => void = () => {};
  const { requests } = mockSearchSetup(
    () => (saved ? { ...off, download_bytes: 0, restart_required: true } : off),
    () => new Promise((resolve) => (finish = resolve)),
  );
  const first = openSearchTab();
  expect(await screen.findByText("Downloads 290 MB once, shared by all Kenn apps")).toBeTruthy();
  expect(requests.some(({ url }) => url.endsWith("/plans"))).toBe(false);

  await fireEvent.click(screen.getByRole("button", { name: "Turn on" }));
  expect(await screen.findByText("Turning on")).toBeTruthy();
  const setup = await waitFor(() => {
    const request = requests.find(({ url }) => url.endsWith("/processing/local/setup"));
    expect(request).toBeTruthy();
    return request;
  });
  expect(JSON.parse(String(setup?.init?.body))).toEqual({
    server: "auto",
    plan_fingerprint: plan.fingerprint,
    consent: true,
  });

  first.unmount();
  expect(setup?.init?.signal?.aborted ?? false).toBe(false);
  openSearchTab();
  expect(await screen.findByText("Turning on")).toBeTruthy();
  saved = true;
  finish(new Response(JSON.stringify(plan)));
  expect(await screen.findByText("Restart required")).toBeTruthy();
  expect(screen.getByText(/to finish\./).textContent).toBe("Run docbank daemon restart to finish.");
  expect(screen.queryByRole("button", { name: "Turn on" })).toBeNull();
  expect(requests.filter(({ url }) => url.endsWith("/processing/local/setup"))).toHaveLength(1);
});

it("keeps the restart instruction until the daemon restarts", async () => {
  mockSearchSetup(
    () => ({ ...off, download_bytes: 0, restart_required: true }),
    () => Promise.reject(new Error("unused")),
  );
  openSearchTab();
  expect(await screen.findByText("Restart required")).toBeTruthy();
  expect(screen.getByText(/to finish\./).textContent).toBe("Run docbank daemon restart to finish.");
});

it("shows a vault on Ollama as on without a switch", async () => {
  mockSearchSetup(
    () => ({ ...off, active: true, provider: "ollama", download_bytes: 0 }),
    () => Promise.reject(new Error("unused")),
  );
  openSearchTab();
  expect(await screen.findByText("On · Ollama")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Use built-in instead" })).toBeNull();
  expect(screen.queryByText("Switching keeps your index")).toBeNull();
});

it("shows a failed setup and retries it", async () => {
  let attempts = 0;
  mockSearchSetup(
    () => off,
    async () =>
      ++attempts === 1
        ? new Response(JSON.stringify({ detail: "Download failed" }), { status: 502 })
        : new Response(JSON.stringify(plan)),
  );
  openSearchTab();
  await fireEvent.click(await screen.findByRole("button", { name: "Turn on" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Download failed");
  await fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  await waitFor(() => expect(attempts).toBe(2));
  expect(await screen.findByText("Downloads 290 MB once, shared by all Kenn apps")).toBeTruthy();
  expect(screen.queryByRole("alert")).toBeNull();
});

it("stops status reads when leaving the tab", async () => {
  const { requests } = mockSearchSetup(
    () => off,
    () => Promise.reject(new Error("unused")),
  );
  openSearchTab();
  await fireEvent.click(screen.getByRole("tab", { name: "Running now" }));
  const status = requests.find(({ url }) => url.endsWith("/processing/local/status"));
  expect(status?.init?.signal?.aborted).toBe(true);
});
