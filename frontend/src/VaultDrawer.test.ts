import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import VaultDrawer from "./VaultDrawer.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

function mockSystemRequests() {
  return vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    if (url === "/api/v1/info") return Response.json({ tracked_blobs: 0 });
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
  ).toEqual(["Disk use", "Running now", "Backups", "Protected folders", "Usage reporting"]);
  expect(within(drawer).getByRole("tab", { name: "Disk use", selected: true })).toBeTruthy();
  expect(await within(drawer).findByText("Total on disk")).toBeTruthy();
});

it("slides in once and stays put while switching tabs", async () => {
  mockSystemRequests();
  render(VaultDrawer, { session: "synthetic", onclose: vi.fn(), onauthfailure: vi.fn() });
  const shell = () => document.querySelector(".shell")!;
  expect(shell().classList.contains("still")).toBe(false);
  await fireEvent.click(screen.getByRole("tab", { name: "Backups" }));
  expect(shell().classList.contains("still")).toBe(true);
  await fireEvent.click(screen.getByRole("tab", { name: "Running now" }));
  expect(shell().classList.contains("still")).toBe(true);
});

it("shows what usage reporting sends and a copyable opt-out", async () => {
  mockSystemRequests();
  render(VaultDrawer, {
    session: "synthetic",
    initialTab: "telemetry",
    onclose: vi.fn(),
    onauthfailure: vi.fn(),
  });
  const page = within(screen.getByRole("region", { name: "Usage reporting" }));
  expect(page.queryByText("On")).toBeNull();
  expect(page.getByText(/search queries stay private/)).toBeTruthy();
  expect(page.getByRole("button", { name: "Copy opt-out command" }).title).toBe(
    "DOCBANK_TELEMETRY_ENABLED=0 docbank daemon restart",
  );
});

it("keeps navigation and close inside the active tool's dialog", async () => {
  mockSystemRequests();
  const onclose = vi.fn();
  render(VaultDrawer, { session: "synthetic", onclose, onauthfailure: vi.fn() });
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
