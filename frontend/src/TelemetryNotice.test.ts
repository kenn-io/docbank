import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import TelemetryNotice from "./TelemetryNotice.svelte";

const reporting = (reason: "enabled" | "disabled" | "checking") => ({
  onQuietChange(listener: (quiet: boolean, reason: "enabled" | "disabled" | "checking") => void) {
    listener(reason !== "enabled", reason);
    return () => {};
  },
});

beforeEach(() => localStorage.clear());
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it("shows once per vault until dismissed", async () => {
  const view = render(TelemetryNotice, { vaultID: "vault-a", reporting: reporting("enabled") });
  await screen.findByText(/anonymous usage data/);
  expect(screen.queryByText("DOCBANK_TELEMETRY_ENABLED=0")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
  expect(screen.queryByRole("complementary")).toBeNull();
  view.unmount();

  render(TelemetryNotice, { vaultID: "vault-a", reporting: reporting("enabled") });
  expect(screen.queryByRole("complementary")).toBeNull();
});

it("still shows for a different vault and when storage is unavailable", async () => {
  localStorage.setItem("docbank:telemetry-notice:v1:vault-a", "dismissed");
  render(TelemetryNotice, { vaultID: "vault-b", reporting: reporting("enabled") });
  await screen.findByRole("complementary", { name: "Anonymous usage" });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
    throw new Error("blocked");
  });
  await fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
  expect(screen.queryByRole("complementary")).toBeNull();
});

it("opens usage reporting and counts as dismissed", async () => {
  const onmanage = vi.fn();
  const view = render(TelemetryNotice, {
    vaultID: "vault-a",
    reporting: reporting("enabled"),
    onmanage,
  });
  await fireEvent.click(await screen.findByRole("button", { name: "System → Usage reporting" }));
  expect(onmanage).toHaveBeenCalledOnce();
  expect(screen.queryByRole("complementary")).toBeNull();
  view.unmount();

  render(TelemetryNotice, { vaultID: "vault-a", reporting: reporting("enabled"), onmanage });
  expect(screen.queryByRole("complementary")).toBeNull();
});

it("stays hidden while the daemon is not accepting usage data", async () => {
  for (const reason of ["disabled", "checking"] as const) {
    const view = render(TelemetryNotice, { vaultID: "vault-a", reporting: reporting(reason) });
    expect(screen.queryByRole("complementary", { name: "Anonymous usage" })).toBeNull();
    view.unmount();
  }
});

it("waits for the supplied vault identity and keeps dismissals scoped to that vault", async () => {
  const view = render(TelemetryNotice, { vaultID: "", reporting: reporting("enabled") });
  expect(screen.queryByRole("complementary")).toBeNull();

  await view.rerender({ vaultID: "vault-a" });
  await fireEvent.click(await screen.findByRole("button", { name: "Dismiss" }));
  expect(localStorage.getItem("docbank:telemetry-notice:v1:vault-a")).toBe("dismissed");

  await view.rerender({ vaultID: "vault-b" });
  expect(await screen.findByRole("complementary", { name: "Anonymous usage" })).toBeTruthy();
  await view.rerender({ vaultID: "vault-a" });
  expect(screen.queryByRole("complementary")).toBeNull();
});
