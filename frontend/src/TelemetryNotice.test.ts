import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import TelemetryNotice from "./TelemetryNotice.svelte";

const status = (vault_id: string) => new Response(JSON.stringify({ vault_id }));

beforeEach(() => localStorage.clear());
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it("shows once per vault until dismissed", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async () => status("vault-a"));
  const view = render(TelemetryNotice, { session: "session" });
  await screen.findByText(/anonymous usage data/);
  expect(screen.queryByText("DOCBANK_TELEMETRY_ENABLED=0")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
  expect(screen.queryByRole("complementary")).toBeNull();
  view.unmount();

  render(TelemetryNotice, { session: "session" });
  await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
  expect(screen.queryByRole("complementary")).toBeNull();
});

it("still shows for a different vault and when storage is unavailable", async () => {
  localStorage.setItem("docbank:telemetry-notice:v1:vault-a", "dismissed");
  vi.spyOn(globalThis, "fetch").mockImplementation(async () => status("vault-b"));
  render(TelemetryNotice, { session: "session" });
  await screen.findByRole("complementary", { name: "Anonymous usage" });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
    throw new Error("blocked");
  });
  await fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
  expect(screen.queryByRole("complementary")).toBeNull();
});

it("opens usage reporting and counts as dismissed", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async () => status("vault-c"));
  const onmanage = vi.fn();
  const view = render(TelemetryNotice, { session: "session", onmanage });
  await fireEvent.click(await screen.findByRole("button", { name: "System → Usage reporting" }));
  expect(onmanage).toHaveBeenCalledOnce();
  expect(screen.queryByRole("complementary")).toBeNull();
  view.unmount();

  render(TelemetryNotice, { session: "session", onmanage });
  await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
  expect(screen.queryByRole("complementary")).toBeNull();
});
