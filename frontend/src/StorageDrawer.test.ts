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
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
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
    expect(screen.getByText(/pending repack/i)).toBeTruthy();
    expect(screen.getByText(/3 loose files/)).toBeTruthy();
    expect(screen.getByText(/12 live packed objects/)).toBeTruthy();
    expect(screen.queryByText(/authorized objects/i)).toBeNull();
    expect(screen.getByText("20% of packs")).toBeTruthy();
    expect(screen.getByText(/Dead packed bytes are not reclaimed space/)).toBeTruthy();

    await fireEvent.click(
      screen.getByRole("button", { name: "Close storage status" }),
    );
    expect(close).toHaveBeenCalledOnce();
  });
});
