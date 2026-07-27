import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
} from "@testing-library/svelte";
import VersionHistoryDrawer from "./VersionHistoryDrawer.svelte";
import type { ContentVersion, Node } from "./api.js";

const currentVersionID = "33333333-3333-4333-8333-333333333333";
const replacedVersionID = "22222222-2222-4222-8222-222222222222";
const createdVersionID = "11111111-1111-4111-8111-111111111111";

const node: Node = {
  id: 42,
  name: "quarterly-report.txt",
  kind: "file",
  current_version_id: currentVersionID,
  blob_hash: "c".repeat(64),
  size: 384,
  mime_type: "text/plain; charset=utf-8",
  revision: 3,
  created_at: "2026-01-02T03:04:05Z",
  modified_at: "2026-07-24T12:00:00Z",
};

function version(
  id: string,
  revision: number,
  transition: ContentVersion["transition_kind"],
  hash: string,
  sourceVersionID?: string,
): ContentVersion {
  return {
    id,
    node_id: node.id,
    blob_hash: hash.repeat(64),
    size: revision * 128,
    mime_type: "text/plain; charset=utf-8",
    recorded_at: `2026-07-${21 + revision}T12:00:00Z`,
    node_revision: revision,
    introduced_operation_id: `${revision}${revision}${revision}${revision}${revision}${revision}${revision}${revision}-1111-4111-8111-111111111111`,
    transition_kind: transition,
    source_version_id: sourceVersionID,
  };
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("version history drawer", () => {
  it("shows newest-first authority and revert lineage", async () => {
    const versions = [
      version(currentVersionID, 3, "content_revert", "c", createdVersionID),
      version(replacedVersionID, 2, "content_replace", "b"),
      version(createdVersionID, 1, "content_create", "a"),
    ];
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          items: versions,
          total: versions.length,
          limit: 1000,
          offset: 0,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      ),
    );
    const close = vi.fn();

    render(VersionHistoryDrawer, {
      session: "short-lived",
      node: { ...node, current_version_id: replacedVersionID },
      path: "/Reports/quarterly-report.txt",
      onclose: close,
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("3 retained versions")).toBeTruthy();
    expect(
      screen.getByText("Current").closest("button")?.getAttribute("aria-label"),
    ).toContain("Reverted");
    expect(screen.getByText(currentVersionID)).toBeTruthy();
    expect(screen.getByText(createdVersionID)).toBeTruthy();
    expect(screen.getByText("Revert source")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Download this version" })).toBeTruthy();
    expect(fetchMock.mock.calls[0]?.[0]).toBe(
      "/api/v1/nodes/42/versions?limit=1000&offset=0",
    );

    const createdHeading = screen.getByText("Created");
    const createdButton = createdHeading.closest("button");
    expect(createdButton).toBeTruthy();
    await fireEvent.click(createdButton!);

    expect(screen.getByText("a".repeat(64))).toBeTruthy();
    expect(screen.queryByText("Revert source")).toBeNull();
    expect(screen.getByRole("button", { name: "Download this version" })).toBeTruthy();

    await fireEvent.click(
      screen.getByRole("button", { name: "Close version history" }),
    );
    expect(close).toHaveBeenCalledOnce();
  });
});
