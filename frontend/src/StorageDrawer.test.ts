import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/svelte";
import StorageDrawer from "./StorageDrawer.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("storage status drawer", () => {
  it("separates dead packed payload from live physical authority", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          loose_blobs: 3,
          loose_bytes: 2048,
          packs: 2,
          pack_stored_bytes: 10240,
          packed_blobs: 12,
          packed_raw_bytes: 16384,
          packed_stored_bytes: 8192,
          dead_packed_bytes: 2048,
          stores: [
            {
              id: "10000000-0000-4000-8000-000000000001",
              name: "primary",
              kind: "filesystem",
              role: "primary",
              lifecycle: "active",
              state: "online",
              priority: 0,
              authoritative_objects: 12,
              logical_bytes: 16384,
              stored_bytes: 10240,
              pack_count: 2,
              dead_packed_bytes: 2048,
              sole_authority_objects: 4,
              affected_documents: 3,
              unreadable_objects: 0,
            },
            {
              id: "10000000-0000-4000-8000-000000000002",
              name: "cold archive",
              kind: "s3",
              role: "secondary",
              lifecycle: "active",
              state: "unavailable",
              priority: 20,
              authoritative_objects: 6,
              logical_bytes: 8192,
              stored_bytes: 6144,
              pack_count: 1,
              dead_packed_bytes: 0,
              sole_authority_objects: 2,
              affected_documents: 2,
              unreadable_objects: 2,
            },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    const close = vi.fn();

    render(StorageDrawer, {
      session: "short-lived",
      onclose: close,
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText(/live packed content/i)).toBeTruthy();
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/storage?refresh=true");
    expect(screen.getByText(/pending repack/i)).toBeTruthy();
    expect(screen.getByText(/3 loose files/)).toBeTruthy();
    expect(screen.getByText(/12 live packed objects/)).toBeTruthy();
    expect(screen.queryByText(/authorized objects/i)).toBeNull();
    expect(screen.getByText("20% of packs")).toBeTruthy();
    expect(screen.getByText("cold archive")).toBeTruthy();
    expect(screen.getByText("unavailable")).toBeTruthy();
    expect(screen.getByText(/2 objects currently have no readable alternative/)).toBeTruthy();
    expect(screen.getByText(/Dead packed bytes are not reclaimed space/)).toBeTruthy();

    await fireEvent.click(
      screen.getByRole("button", { name: "Close storage status" }),
    );
    expect(close).toHaveBeenCalledOnce();
  });
});
