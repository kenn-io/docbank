import { afterEach, describe, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
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
  it("keeps a directly fetched version pinned outside the newest 1000", async () => {
    const versions = Array.from({ length: 1000 }, (_, index) =>
      version(
        `${(index + 1).toString(16).padStart(8, "0")}-2222-4222-8222-222222222222`,
        1001 - index,
        "content_replace",
        "c",
      ),
    );
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      Response.json({ items: versions, total: 1001, limit: 1000, offset: 0 }),
    );
    render(VersionHistoryDrawer, {
      session: "short-lived",
      node: {
        ...node,
        current_version_id: versions[0].id,
        revision: 1001,
        trashed_at: "2026-10-01T00:00:00Z",
      },
      path: "/Reports/quarterly-report.txt",
      pinnedVersion: version(createdVersionID, 1, "content_create", "a"),
      onselect: vi.fn(),
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    await screen.findByText("1001 retained versions");
    expect(screen.getAllByText(createdVersionID).length).toBeGreaterThan(0);
    expect(screen.getByText("a".repeat(64))).toBeTruthy();
    expect(screen.getByText("128 B (128 bytes)")).toBeTruthy();
    expect(screen.getByText("Trash")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Download this version" })).toBeNull();
    expect(
      screen.getByText("Restore this file before downloading versions or email PDFs."),
    ).toBeTruthy();
  });

  it("hides email PDF actions for a trashed node", async () => {
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

    const drawer = render(VersionHistoryDrawer, {
      session: "short-lived",
      pinnedVersion: versions[0],
      node: { ...node, current_version_id: replacedVersionID },
      path: "/Reports/quarterly-report.txt",
      onselect: vi.fn(),
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
    await drawer.rerender({ pinnedVersion: versions[2] });

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

    const drawer = render(VersionHistoryDrawer, {
      session: "short-lived",
      pinnedVersion: versions[0],
      node,
      path: "/Reports/quarterly-report.txt",
      onselect: vi.fn(),
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("2 retained versions")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Download this version" }));
    await vi.waitFor(() => expect(downloadSignal).toBeTruthy());

    await fireEvent.click(screen.getByText("Created").closest("button")!);
    await drawer.rerender({ pinnedVersion: versions[1] });

    await vi.waitFor(() => expect(downloadSignal?.aborted).toBe(true));
  });
});

it("waits for the parent to accept the newest version and later clicks", async () => {
  const newest = version(currentVersionID, 3, "content_replace", "c");
  const older = version(createdVersionID, 1, "content_create", "a");
  vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(Response.json({ items: [newest, older], total: 2, limit: 1000, offset: 0 }));
  const onselect = vi.fn();
  const drawer = render(VersionHistoryDrawer, { session: "short-lived", node, path: "/Reports/example.txt", onselect, onclose: vi.fn(), onauthfailure: vi.fn() });
  await screen.findByText("2 retained versions");
  expect(onselect).toHaveBeenCalledWith(newest);
  expect(screen.queryByText(currentVersionID)).toBeNull();
  await drawer.rerender({ pinnedVersion: newest });
  expect(screen.getByText(currentVersionID)).toBeTruthy();
  await fireEvent.click(screen.getByText("Created").closest("button")!);
  expect(onselect).toHaveBeenLastCalledWith(older);
  expect(screen.queryByText(createdVersionID)).toBeNull();
  await drawer.rerender({ pinnedVersion: older });
  expect(screen.getByText(createdVersionID)).toBeTruthy();
});
