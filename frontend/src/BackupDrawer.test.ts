import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/svelte";
import BackupDrawer from "./BackupDrawer.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("backup snapshots drawer", () => {
  it("shows configured recovery points newest first without claiming verification", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          repository: {
            id: "11111111-1111-4111-8111-111111111111",
            path: "/var/backups/docbank",
          },
          items: [
            {
              id: "01JOLD",
              created_at: "2026-07-20T12:00:00Z",
              tag: "first import",
              metadata_format: "docbank-jsonl-v1",
              nodes: 12,
              files: 10,
              blobs: 10,
              blob_bytes: 4096,
              packs_added: 1,
              bytes_added: 2048,
              duration_seconds: 1.2,
            },
            {
              id: "01JNEW",
              parent_id: "01JOLD",
              created_at: "2026-07-27T12:00:00Z",
              tag: "weekly",
              metadata_format: "docbank-jsonl-v1",
              nodes: 14,
              files: 12,
              blobs: 12,
              blob_bytes: 8192,
              packs_added: 1,
              bytes_added: 1024,
              duration_seconds: 0.8,
            },
          ],
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    const close = vi.fn();

    render(BackupDrawer, {
      session: "short-lived",
      onclose: close,
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("/var/backups/docbank")).toBeTruthy();
    expect(
      screen
        .getByText("weekly")
        .compareDocumentPosition(screen.getByText("first import")) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(screen.getByText("Incremental")).toBeTruthy();
    expect(screen.getByText("Full")).toBeTruthy();
    expect(screen.getByText(/Snapshot presence is not recovery proof/)).toBeTruthy();

    await fireEvent.click(
      screen.getByRole("button", { name: "Close backup snapshots" }),
    );
    expect(close).toHaveBeenCalledOnce();
  });
});
