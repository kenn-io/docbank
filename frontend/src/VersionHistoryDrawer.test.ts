import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import VersionHistoryDrawer from "./VersionHistoryDrawer.svelte";
import { node, version } from "./version-test-fixtures.js";

const currentVersionID = "33333333-3333-4333-8333-333333333333";
const replacedVersionID = "22222222-2222-4222-8222-222222222222";
const createdVersionID = "11111111-1111-4111-8111-111111111111";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("version history drawer", () => {
  it("shows trashed-node actions and requires restore before downloads", async () => {
    const emailVersion = {
      ...version(createdVersionID, 1, "content_create", "a"),
      mime_type: "message/rfc822",
    };
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      if (String(input).includes("/api/v1/email-pdfs/")) return Response.json([]);
      return Response.json({
        items: [emailVersion],
        total: 1,
        limit: 1000,
        offset: 0,
      });
    });
    render(VersionHistoryDrawer, {
      session: "short-lived",
      node: {
        ...node,
        current_version_id: emailVersion.id,
        revision: emailVersion.node_revision,
        blob_hash: emailVersion.blob_hash,
        name: "message.eml",
        mime_type: "message/rfc822",
        trashed_at: "2026-10-01T00:00:00Z",
      },
      path: "/Reports/message.eml",
      pinnedVersion: emailVersion,
      onselect: vi.fn(),
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("1 retained version")).toBeTruthy();
    expect(screen.getByText("Trash")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Exact email PDF" })).toBeNull();
    expect(
      screen.getByText("Restore this file before downloading versions or email PDFs."),
    ).toBeTruthy();
  });

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
    expect(screen.getByText("Current").closest("button")?.getAttribute("aria-label")).toContain(
      "Reverted",
    );
    expect(screen.getByText(currentVersionID)).toBeTruthy();
    expect(screen.getByText(createdVersionID)).toBeTruthy();
    expect(screen.getByText("Revert source")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Download this version" })).toBeTruthy();
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/nodes/42/versions?limit=1000&offset=0");

    const createdHeading = screen.getByText("Created");
    const createdButton = createdHeading.closest("button");
    expect(createdButton).toBeTruthy();
    await fireEvent.click(createdButton!);

    expect(screen.getByText("a".repeat(64))).toBeTruthy();
    expect(screen.queryByText("Revert source")).toBeNull();
    expect(screen.getByRole("button", { name: "Download this version" })).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Close version history" }));
    expect(close).toHaveBeenCalledOnce();
  });

  it("aborts a retained-version download when the selection changes", async () => {
    const versions = [
      version(currentVersionID, 3, "content_replace", "c"),
      version(createdVersionID, 1, "content_create", "a"),
    ];
    let downloadSignal: AbortSignal | undefined;
    vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            items: versions,
            total: versions.length,
            limit: 1000,
            offset: 0,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
      )
      .mockImplementationOnce((_input, init) => {
        downloadSignal = init?.signal ?? undefined;
        return new Promise((_resolve, reject) => {
          downloadSignal?.addEventListener(
            "abort",
            () => reject(new DOMException("aborted", "AbortError")),
            { once: true },
          );
        });
      });

    render(VersionHistoryDrawer, {
      session: "short-lived",
      node,
      path: "/Reports/quarterly-report.txt",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("2 retained versions")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Download this version" }));
    await vi.waitFor(() => expect(downloadSignal).toBeTruthy());

    await fireEvent.click(screen.getByText("Created").closest("button")!);

    await vi.waitFor(() => expect(downloadSignal?.aborted).toBe(true));
  });

  it("reloads on revision changes only in linked mode and hides unresolved selections", async () => {
    const newest = version(currentVersionID, 3, "content_replace", "c");
    const older = version(createdVersionID, 1, "content_create", "a");
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockImplementation(async () =>
        Response.json({ items: [newest, older], total: 2, limit: 1000, offset: 0 }),
      );
    const drawer = render(VersionHistoryDrawer, {
      session: "short-lived",
      node,
      path: "/Reports/example.txt",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await screen.findByText("2 retained versions");
    await fireEvent.click(screen.getByText("Created").closest("button")!);
    expect(screen.getByText(createdVersionID)).toBeTruthy();

    await drawer.rerender({ node: { ...node, revision: node.revision + 1 } });
    expect(screen.getByText(createdVersionID)).toBeTruthy();
    expect(fetchMock.mock.calls.map(([input]) => String(input))).toEqual([
      "/api/v1/nodes/42/versions?limit=1000&offset=0",
    ]);

    drawer.unmount();
    fetchMock.mockClear();
    const linkedDrawer = render(VersionHistoryDrawer, {
      session: "short-lived",
      node,
      path: "/Reports/example.txt",
      onselect: vi.fn(),
      pinnedVersion: newest,
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await screen.findByText("2 retained versions");
    fetchMock.mockClear();

    const replacementID = "44444444-4444-4444-8444-444444444444";
    const replacement = version(replacementID, 5, "content_replace", "d");
    fetchMock.mockResolvedValue(
      Response.json({ items: [replacement, newest, older], total: 3, limit: 1000, offset: 0 }),
    );
    await linkedDrawer.rerender({
      node: {
        ...node,
        revision: 5,
        current_version_id: replacementID,
        blob_hash: replacement.blob_hash,
      },
      pinnedVersion: newest,
      resolving: true,
    });
    expect(screen.queryByText(currentVersionID)).toBeNull();
    expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
    const timeline = within(screen.getByRole("region", { name: "Retained versions" }));
    await timeline.findByText("Revision 5");
    expect(
      within(timeline.getByText("Current").closest("button")!).getByText("Revision 5"),
    ).toBeTruthy();
    expect(fetchMock.mock.calls.map(([input]) => String(input))).toEqual([
      "/api/v1/nodes/42/versions?limit=1000&offset=0",
    ]);
    await linkedDrawer.rerender({ resolving: false, unavailable: "Version lookup unavailable" });
    expect(screen.getByText("Version lookup unavailable")).toBeTruthy();
    expect(screen.queryByText(currentVersionID)).toBeNull();
    expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
  });

  it("keeps the pinned version until the parent accepts a linked selection", async () => {
    const newest = version(currentVersionID, 3, "content_replace", "c");
    const older = version(createdVersionID, 1, "content_create", "a");
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        Response.json({ items: [newest, older], total: 2, limit: 1000, offset: 0 }),
      );
    const onselect = vi.fn();
    const drawer = render(VersionHistoryDrawer, {
      session: "short-lived",
      node,
      path: "/Reports/example.txt",
      pinnedVersion: older,
      onselect,
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await screen.findByText("2 retained versions");
    expect(onselect).not.toHaveBeenCalled();
    expect(screen.getByText(createdVersionID)).toBeTruthy();
    expect(screen.queryByText(currentVersionID)).toBeNull();
    await fireEvent.click(screen.getByText("Replaced").closest("button")!);
    expect(onselect).toHaveBeenCalledExactlyOnceWith(newest);
    expect(screen.getByText(createdVersionID)).toBeTruthy();
    expect(screen.queryByText(currentVersionID)).toBeNull();
    fetchMock.mockClear();
    await drawer.rerender({ pinnedVersion: newest });
    expect(screen.getByText(currentVersionID)).toBeTruthy();
    expect(screen.queryByText(createdVersionID)).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
