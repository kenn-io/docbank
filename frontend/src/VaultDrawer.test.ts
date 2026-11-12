import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import VaultDrawer, { type SearchByMeaningSetup } from "./VaultDrawer.svelte";

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
  render(VaultDrawer, {
    session: "synthetic",
    ...searchProps(),
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
  render(VaultDrawer, { session: "synthetic", ...searchProps(), onclose, onauthfailure: vi.fn() });
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

it("keeps keyboard focus on the selected tab when switching drawer contents", async () => {
  mockSystemRequests();
  render(VaultDrawer, {
    session: "synthetic",
    ...searchProps(),
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  const disk = screen.getByRole("tab", { name: "Disk use" });
  disk.focus();
  await fireEvent.keyDown(disk, { key: "ArrowRight" });
  const jobs = screen.getByRole("tab", { name: "Running now", selected: true });
  await vi.waitFor(() => expect(document.activeElement).toBe(jobs));
  await fireEvent.keyDown(jobs, { key: "ArrowRight" });
  const backups = screen.getByRole("tab", { name: "Backups", selected: true });
  await vi.waitFor(() => expect(document.activeElement).toBe(backups));
});

const off = {
  active: false,
  provider: "",
  download_bytes: 290_000_000,
  restart_required: false,
} as const;
const offLine = "Downloads 290 MB once, shared by all Kenn apps";

function searchProps(searchByMeaning: Partial<SearchByMeaningSetup> = {}) {
  return {
    searchByMeaning: { readError: "", pending: false, error: "", ...searchByMeaning },
    onreadsearchbymeaning: vi.fn(() => vi.fn()),
    onturnonsearchbymeaning: vi.fn(),
    onrestart: vi.fn(async () => {}),
  };
}

function openSearchTab(searchByMeaning: Partial<SearchByMeaningSetup>) {
  const props = {
    session: "synthetic",
    initialTab: "searchByMeaning",
    ...searchProps(searchByMeaning),
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  };
  render(VaultDrawer, props);
  return props;
}

it.each<[string, Partial<SearchByMeaningSetup>, string, boolean]>([
  ["off with the download size", { status: off }, offLine, true],
  [
    "off with the server downloaded",
    { status: { ...off, download_bytes: 0 } },
    "Already on this computer",
    true,
  ],
  ["off after a failed setup", { status: off, error: "Download failed" }, offLine, true],
  ["turning on", { status: off, pending: true }, "Turning on", false],
  [
    "restart required",
    { status: { ...off, download_bytes: 0, restart_required: true } },
    "Restart required",
    false,
  ],
  ["on", { status: { ...off, active: true, provider: "local", download_bytes: 0 } }, "On", false],
  [
    "on Ollama",
    { status: { ...off, active: true, provider: "ollama", download_bytes: 0 } },
    "On · Ollama",
    false,
  ],
])("shows search by meaning %s", async (_, setup, text, turnOn) => {
  const props = openSearchTab(setup);
  expect(screen.getByText(text)).toBeTruthy();
  if (turnOn) {
    await fireEvent.click(screen.getByRole("button", { name: "Turn on" }));
    expect(props.onturnonsearchbymeaning).toHaveBeenCalledOnce();
  } else expect(screen.queryByRole("button", { name: "Turn on" })).toBeNull();
  expect(screen.queryByRole("alert")?.textContent ?? "").toBe(setup.error ?? "");
  expect(screen.queryByRole("button", { name: "Restart" }) !== null).toBe(
    !!setup.status?.restart_required,
  );
  expect(screen.queryByRole("button", { name: "Use built-in instead" })).toBeNull();
  expect(screen.queryByText("Switching keeps your index")).toBeNull();
});

it("retries a failed status read", async () => {
  const props = openSearchTab({ readError: "Daemon busy" });
  expect(screen.getByRole("alert").textContent).toBe("Daemon busy");
  await fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(props.onreadsearchbymeaning).toHaveBeenCalledTimes(2);
});

it("keeps a failed setup visible beside a failed status read", () => {
  openSearchTab({ status: off, error: "Download failed", readError: "Daemon busy" });
  expect(screen.getByText(offLine)).toBeTruthy();
  expect(screen.getAllByRole("alert").map((alert) => alert.textContent)).toEqual([
    "Download failed",
    "Daemon busy",
  ]);
  expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
});

it("keeps the restart step beside a failed status read", () => {
  openSearchTab({
    status: { ...off, download_bytes: 0, restart_required: true },
    readError: "Daemon busy",
  });
  expect(screen.getByRole("alert").textContent).toBe("Daemon busy");
  expect(screen.getByRole("button", { name: "Restart" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
});

it("reads status on entering the tab and stops when leaving it", async () => {
  mockSystemRequests();
  const props = openSearchTab({});
  expect(props.onreadsearchbymeaning).toHaveBeenCalledOnce();
  const stop = props.onreadsearchbymeaning.mock.results[0]?.value;
  expect(stop).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("tab", { name: "Running now" }));
  expect(stop).toHaveBeenCalledOnce();
});

it("confirms a restart and disables both actions while it is pending", async () => {
  let finish!: () => void;
  const props = openSearchTab({
    status: { ...off, download_bytes: 0, restart_required: true },
  });
  props.onrestart.mockImplementation(
    () =>
      new Promise<void>((resolve) => {
        finish = resolve;
      }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Restart" }));
  let dialog = screen.getByRole("dialog", { name: "Restart Docbank?" });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  expect(props.onrestart).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button", { name: "Restart" }));
  dialog = screen.getByRole("dialog", { name: "Restart Docbank?" });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Restart" }));
  expect(props.onrestart).toHaveBeenCalledOnce();
  expect(
    (within(dialog).getByRole("button", { name: "Restarting…" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  expect(
    (within(dialog).getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled,
  ).toBe(true);
  finish();
});
