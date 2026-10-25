import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import StorageDrawer from "./StorageDrawer.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("storage status drawer", () => {
  it("shows disk use, one row per location, and the storage internals without a disclosure", async () => {
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

    expect(await screen.findByText("Total on disk")).toBeTruthy();
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/storage?refresh=true");
    const tiles = within(screen.getByRole("group", { name: "Disk use summary" }));
    expect(tiles.getByText("Total on disk").nextElementSibling?.textContent).toBe("12.0 KiB");
    expect(tiles.getByText("Files stored").nextElementSibling?.textContent).toBe("12");
    expect(tiles.getByText("Reclaimable").nextElementSibling?.textContent).toBe(
      "2.00 KiB (20% of packs)",
    );
    expect(screen.queryByText(/authorized objects/i)).toBeNull();
    expect(document.querySelector("details")).toBeNull();

    const locations = within(screen.getByRole("region", { name: "Storage locations" }));
    const [header, primary, cold] = locations.getAllByRole("row");
    expect(
      within(header!)
        .getAllByRole("columnheader")
        .map((cell) => cell.textContent),
    ).toEqual(["Name", "Kind", "Size", "Objects", "Only copy", "Status"]);
    expect(
      within(primary!)
        .getAllByRole("cell")
        .map((cell) => cell.textContent?.trim()),
    ).toEqual(["filesystem", "10.0 KiB", "12", "4", "online"]);
    expect(within(primary!).getByRole("rowheader").textContent).toBe("primary");
    expect(locations.queryByText(/Primary ·/)).toBeNull();
    expect(within(cold!).getByRole("rowheader").textContent).toBe("cold archive");
    expect(locations.getByText("unavailable")).toBeTruthy();
    expect(locations.getByText(/2 objects currently have no readable alternative/)).toBeTruthy();

    const internals = within(screen.getByLabelText("Storage breakdown"));
    expect(internals.getByText("Loose files").nextElementSibling?.textContent).toBe("3 · 2.00 KiB");
    expect(internals.getByText("Waiting to compact").nextElementSibling?.textContent).toBe(
      "2.00 KiB",
    );

    await fireEvent.click(screen.getByRole("button", { name: "Close storage status" }));
    expect(close).toHaveBeenCalledOnce();
  });
});
