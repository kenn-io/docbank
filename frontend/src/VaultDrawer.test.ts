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
        JSON.stringify({
          repository: { id: "demo", path: "/demo/backups" },
          items: [],
        }),
      );
    if (url.includes("/jobs")) return new Response(JSON.stringify({ items: [], lanes: [] }));
    throw new Error(`Unexpected request ${url}`);
  });
}

it("lists the sections in order and opens on disk use", async () => {
  mockSystemRequests();
  render(VaultDrawer, {
    session: "synthetic",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
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
  render(VaultDrawer, {
    session: "synthetic",
    onclose,
    onauthfailure: vi.fn(),
  });
  await fireEvent.click(screen.getByRole("tab", { name: "Backups" }));
  const drawer = screen.getByRole("dialog", { name: "System" });
  await fireEvent.click(within(drawer).getByRole("tab", { name: "Usage reporting" }));
  const privacy = screen.getByRole("dialog", { name: "System" });
  expect(within(privacy).getByRole("region", { name: "Usage reporting" })).toBeTruthy();
  await fireEvent.click(within(privacy).getByRole("tab", { name: "Running now" }));
  const activityTab = await screen.findByRole("tab", {
    name: "Running now",
    selected: true,
  });
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
  restart_required: false,
};
const plan = { fingerprint: "f".repeat(64), server: "local", flow: [] };
const offLine = "Downloads 290 MB once, shared by all Kenn apps";

function mockSearchSetup(status: () => Response | object, apply: () => Promise<Response>) {
  const requests: { url: string; init?: RequestInit }[] = [];
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    requests.push({ url, init });
    if (url.endsWith("/processing/local/status")) {
      const next = status();
      return next instanceof Response ? next : new Response(JSON.stringify(next));
    }
    if (url.endsWith("/processing/local/plans")) return new Response(JSON.stringify(plan));
    if (url.endsWith("/processing/local/setup")) return apply();
    if (url.includes("/jobs")) return new Response(JSON.stringify({ items: [], lanes: [] }));
    throw new Error(`Unexpected request ${url}`);
  });
  return { fetch, requests };
}

// Setup is kept per session across drawers, so each test uses its own session.
let sessions = 0;
function openSearchTab(session = `synthetic-${++sessions}`) {
  const props = {
    session,
    initialTab: "searchByMeaning",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  };
  return { ...render(VaultDrawer, props), ...props };
}

function deferred() {
  let resolve: (response: Response) => void = () => {};
  const promise = new Promise<Response>((done) => (resolve = done));
  return { promise, resolve };
}

it.each([
  ["off with the download size", off, offLine, "Turn on"],
  [
    "off with the server downloaded",
    { ...off, download_bytes: 0 },
    "Already on this computer",
    "Turn on",
  ],
  ["on", { ...off, active: true, provider: "local", download_bytes: 0 }, "On", undefined],
])("shows search by meaning %s", async (_, status, text, button) => {
  mockSearchSetup(
    () => status,
    () => Promise.reject(new Error("unused")),
  );
  openSearchTab();
  expect(await screen.findByText(text)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Turn on" }) ? "Turn on" : undefined).toBe(button);
  expect(screen.queryByText(/daemon restart/)).toBeNull();
});

it("turns on search by meaning and keeps setup running after the drawer closes", async () => {
  let saved = false;
  const apply = deferred();
  const { requests } = mockSearchSetup(
    () => (saved ? { ...off, download_bytes: 0, restart_required: true } : off),
    () => apply.promise,
  );
  const first = openSearchTab();
  expect(await screen.findByText(offLine)).toBeTruthy();
  expect(requests.some(({ url }) => url.endsWith("/plans"))).toBe(false);

  await fireEvent.click(screen.getByRole("button", { name: "Turn on" }));
  expect(await screen.findByText("Turning on")).toBeTruthy();
  const setup = await waitFor(() => {
    const request = requests.find(({ url }) => url.endsWith("/processing/local/setup"));
    expect(request).toBeTruthy();
    return request;
  });
  const order = requests.map(({ url }) => url).filter((url) => !url.endsWith("/status"));
  expect(order.map((url) => url.slice(url.lastIndexOf("/") + 1))).toEqual(["plans", "setup"]);
  expect(JSON.parse(String(setup?.init?.body))).toEqual({
    server: "auto",
    plan_fingerprint: plan.fingerprint,
    consent: true,
  });

  first.unmount();
  expect(setup?.init?.signal?.aborted ?? false).toBe(false);
  openSearchTab(first.session);
  expect(await screen.findByText("Turning on")).toBeTruthy();
  saved = true;
  apply.resolve(new Response(JSON.stringify(plan)));
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

it("shows a setup failure that happened while the drawer was closed until the next Turn on", async () => {
  const apply = deferred();
  let attempts = 0;
  mockSearchSetup(
    () => off,
    () => (++attempts === 1 ? apply.promise : Promise.resolve(new Response(JSON.stringify(plan)))),
  );
  const first = openSearchTab();
  await fireEvent.click(await screen.findByRole("button", { name: "Turn on" }));
  await waitFor(() => expect(attempts).toBe(1));
  first.unmount();
  apply.resolve(
    new Response(JSON.stringify({ detail: "Download failed" }), {
      status: 502,
    }),
  );

  openSearchTab(first.session);
  expect((await screen.findByRole("alert")).textContent).toBe("Download failed");
  expect(await screen.findByText(offLine)).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Turn on" }));
  await waitFor(() => expect(attempts).toBe(2));
  expect(await screen.findByText(offLine)).toBeTruthy();
  expect(screen.queryByRole("alert")).toBeNull();
});

it.each([
  ["On", { ...off, active: true, provider: "local", download_bytes: 0 }],
  ["Restart required", { ...off, download_bytes: 0, restart_required: true }],
])("drops a setup failure once status reads %s", async (text, fixed) => {
  let status = off;
  mockSearchSetup(
    () => status,
    () =>
      Promise.resolve(
        new Response(JSON.stringify({ detail: "Download failed" }), {
          status: 502,
        }),
      ),
  );
  const first = openSearchTab();
  await fireEvent.click(await screen.findByRole("button", { name: "Turn on" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Download failed");
  first.unmount();

  status = fixed;
  openSearchTab(first.session);
  expect(await screen.findByText(text)).toBeTruthy();
  expect(screen.queryByRole("alert")).toBeNull();
});

it("retries a failed status read", async () => {
  let reads = 0;
  mockSearchSetup(
    () =>
      ++reads === 1
        ? new Response(JSON.stringify({ detail: "Daemon busy" }), {
            status: 503,
          })
        : off,
    () => Promise.reject(new Error("unused")),
  );
  openSearchTab();
  expect((await screen.findByRole("alert")).textContent).toBe("Daemon busy");
  await fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(await screen.findByText(offLine)).toBeTruthy();
  expect(screen.queryByRole("alert")).toBeNull();
});

it.each([
  [
    "reading status",
    () => new Response("{}", { status: 401 }),
    () => Promise.reject(new Error("unused")),
  ],
  ["turning on", () => off, () => Promise.resolve(new Response("{}", { status: 401 }))],
])("hands a rejected session to sign-in while %s", async (step, status, apply) => {
  mockSearchSetup(status, apply);
  const drawer = openSearchTab();
  if (step === "turning on")
    await fireEvent.click(await screen.findByRole("button", { name: "Turn on" }));
  await waitFor(() => expect(drawer.onauthfailure).toHaveBeenCalledOnce());
  expect(drawer.onclose).toHaveBeenCalledOnce();
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
