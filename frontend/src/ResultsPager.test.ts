import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import ResultsPager from "./ResultsPager.svelte";
import type { SnapshotPage } from "./snapshots.js";

afterEach(cleanup);

function page(overrides: Partial<SnapshotPage> = {}): SnapshotPage {
  return {
    query: { v: 1, text: "", syntax: "simple", mode: "lexical", filters: {}, sort: { field: "name", direction: "asc" } },
    dependencies: [], query_fingerprint: `sha256:${"a".repeat(64)}`, member_hash: "b".repeat(64),
    snapshot_fingerprint: `sha256:${"c".repeat(64)}`, generation: { kind: "native" },
    coverage: { configuration: "unconfigured" }, observed_at: "2026-09-11T12:34:00Z",
    page_size: 50, total: 123, total_bytes: 12345, rows: [], facets: [], snapshot: true,
    snapshot_id: "0123456789abcdef0123456789abcdef", created_at: "2026-09-11T12:34:00Z",
    expires_at: "2026-09-11T13:04:00Z", previous_cursor: "previous", next_cursor: "next", ...overrides,
  };
}

it("reports the exact frozen range, count, bytes, and observation time", () => {
  render(ResultsPager, { page: page({ rows: Array(50).fill({}) }), offset: 50, status: "ready", onpage: vi.fn(), onrunagain: vi.fn() });
  const status = screen.getByRole("status");
  expect(status.textContent).toContain("51–100 of 123");
  expect(status.textContent).toContain("12,345 bytes");
  expect(status.textContent).toContain("2026-09-11T12:34:00Z");
});

it("moves only through supplied cursors and offers an explicit rerun after expiry", async () => {
  const onpage = vi.fn();
  const onrunagain = vi.fn();
  const { rerender } = render(ResultsPager, { page: page({ rows: Array(23).fill({}), next_cursor: undefined }), offset: 100, status: "ready", onpage, onrunagain });
  expect(screen.getByText("101–123 of 123", { exact: false })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Previous page" }));
  expect(onpage).toHaveBeenCalledWith("previous");
  expect((screen.getByRole("button", { name: "Next page" }) as HTMLButtonElement).disabled).toBe(true);
  await rerender({ page: page({ rows: Array(50).fill({}) }), offset: 50, status: "ready" });
  expect(screen.getByText("51–100 of 123", { exact: false })).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Next page" }));
  expect(onpage).toHaveBeenLastCalledWith("next");
  await rerender({ status: "expired" });
  const rerun = screen.getByRole("button", { name: "Run again" });
  expect((rerun as HTMLButtonElement).disabled).toBe(false);
  await fireEvent.click(rerun);
  expect(onrunagain).toHaveBeenCalledOnce();
});
