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

function rows() {
  return within(screen.getByRole("list", { name: "Versions" })).getAllByRole("listitem");
}

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
      path: "message.eml",
      pinnedVersion: emailVersion,
      onselect: vi.fn(),
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("1 version")).toBeTruthy();
    expect(screen.getByText("Trash")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Download" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Exact email PDF" })).toBeNull();
    expect(
      screen.getByText("Restore this file before downloading versions or email PDFs."),
    ).toBeTruthy();
  });

  it("lists versions newest first with a download each and details tucked away", async () => {
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

    expect(await screen.findByText("3 versions")).toBeTruthy();
    expect(fetchMock.mock.calls[0]?.[0]).toBe("/api/v1/nodes/42/versions?limit=1000&offset=0");
    const list = rows();
    expect(list.map((row) => row.querySelector("strong")?.textContent)).toEqual([
      "Restored",
      "Replaced",
      "Created",
    ]);
    expect(within(list[0]!).getByText("Current")).toBeTruthy();
    expect(within(list[1]!).queryByText("Current")).toBeNull();
    expect(screen.getAllByRole("button", { name: "Download" })).toHaveLength(3);
    for (const jargon of ["Immutable", "authority", "Canonical", "Node revision", "Operation"])
      expect(screen.queryByText(new RegExp(jargon, "i"))).toBeNull();
    expect(screen.queryByText(currentVersionID)).toBeNull();

    await fireEvent.click(within(list[0]!).getByRole("button", { name: /^Details for Restored/ }));
    const details = within(screen.getByRole("list", { name: "Versions" }));
    expect(details.getByText(currentVersionID)).toBeTruthy();
    expect(details.getByText("c".repeat(64))).toBeTruthy();
    expect(details.getByText("Restored from")).toBeTruthy();
    expect(details.getByText(createdVersionID)).toBeTruthy();
    expect(screen.queryByText("b".repeat(64))).toBeNull();

    await fireEvent.click(within(list[2]!).getByRole("button", { name: /^Details for Created/ }));
    expect(screen.getByText("a".repeat(64))).toBeTruthy();
    await fireEvent.click(within(list[0]!).getByRole("button", { name: /^Details for Restored/ }));
    expect(screen.queryByText(currentVersionID)).toBeNull();

    await fireEvent.click(screen.getByRole("button", { name: "Close version history" }));
    expect(close).toHaveBeenCalledOnce();
  });

  it("aborts a retained-version download when the drawer closes", async () => {
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

    const view = render(VersionHistoryDrawer, {
      session: "short-lived",
      node,
      path: "/Reports/quarterly-report.txt",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("2 versions")).toBeTruthy();
    await fireEvent.click(within(rows()[1]!).getByRole("button", { name: "Download" }));
    await vi.waitFor(() => expect(downloadSignal).toBeTruthy());

    view.unmount();

    await vi.waitFor(() => expect(downloadSignal?.aborted).toBe(true));
  });

  it("refreshes linked history when the same node has a new current version", async () => {
    const older = version(createdVersionID, 1, "content_create", "a");
    const previous = version(replacedVersionID, 2, "content_replace", "b");
    const newest = version(currentVersionID, 4, "content_replace", "c");
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(
        Response.json({ items: [previous, older], total: 2, limit: 1000, offset: 0 }),
      )
      .mockResolvedValueOnce(
        Response.json({ items: [newest, previous, older], total: 3, limit: 1000, offset: 0 }),
      );
    const onselect = vi.fn();
    const drawer = render(VersionHistoryDrawer, {
      session: "short-lived",
      node: {
        ...node,
        current_version_id: previous.id,
        blob_hash: previous.blob_hash,
        size: previous.size,
        revision: 2,
      },
      path: "/Reports/example.txt",
      pinnedVersion: older,
      onselect,
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await screen.findByText("2 versions");
    expect(within(rows()[0]!).getByText("Current")).toBeTruthy();

    await drawer.rerender({
      node: {
        ...node,
        current_version_id: previous.id,
        blob_hash: previous.blob_hash,
        size: previous.size,
        revision: 3,
      },
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);

    await drawer.rerender({
      node: {
        ...node,
        current_version_id: newest.id,
        blob_hash: newest.blob_hash,
        size: newest.size,
        revision: 4,
      },
    });
    await screen.findByText("3 versions");
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1]?.[0]).toBe("/api/v1/nodes/42/versions?limit=1000&offset=0");
    expect(within(rows()[0]!).getByText("Current")).toBeTruthy();
    expect(within(rows()[1]!).queryByText("Current")).toBeNull();
    const open = screen.getAllByRole("button", { expanded: true });
    expect(open).toHaveLength(1);
    expect(open[0]!.getAttribute("aria-label")).toMatch(/^Details for Created/);
    expect(screen.getByText(createdVersionID)).toBeTruthy();
    expect(onselect).not.toHaveBeenCalled();
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
    await screen.findByText("2 versions");
    expect(onselect).not.toHaveBeenCalled();
    expect(screen.getByText(createdVersionID)).toBeTruthy();
    expect(screen.queryByText(currentVersionID)).toBeNull();
    await fireEvent.click(screen.getByRole("button", { name: /^Details for Replaced/ }));
    expect(onselect).toHaveBeenCalledExactlyOnceWith(newest);
    expect(screen.getByText(createdVersionID)).toBeTruthy();
    expect(screen.queryByText(currentVersionID)).toBeNull();
    fetchMock.mockClear();
    await drawer.rerender({ pinnedVersion: newest });
    expect(screen.getByText(currentVersionID)).toBeTruthy();
    expect(screen.queryByText(createdVersionID)).toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
    await drawer.rerender({ resolving: true });
    expect(screen.queryByText(currentVersionID)).toBeNull();
    expect(screen.getByText("Loading linked version…")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Download" })).toBeNull();
    await drawer.rerender({ resolving: false, unavailable: "Version lookup unavailable" });
    expect(screen.getByText("Version lookup unavailable")).toBeTruthy();
    expect(screen.queryByText(currentVersionID)).toBeNull();
    expect(screen.queryByRole("button", { name: "Download" })).toBeNull();
    await drawer.rerender({ pinnedVersion: newest, unavailable: "" });
    expect(screen.getAllByRole("button", { name: "Download" })).toHaveLength(2);
  });

  it("lists a linked version older than the loaded history first", async () => {
    const newest = version(currentVersionID, 3, "content_replace", "c");
    const older = version(createdVersionID, 1, "content_create", "a");
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      Response.json({ items: [newest], total: 2, limit: 1000, offset: 0 }),
    );
    render(VersionHistoryDrawer, {
      session: "short-lived",
      node,
      path: "/Reports/example.txt",
      pinnedVersion: older,
      onselect: vi.fn(),
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });
    await screen.findByText("2 versions");
    expect(rows()).toHaveLength(2);
    expect(within(rows()[0]!).getByText("Created")).toBeTruthy();
    expect(screen.getByText(createdVersionID)).toBeTruthy();
  });
});
