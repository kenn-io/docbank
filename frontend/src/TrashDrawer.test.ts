import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import TrashDrawer from "./TrashDrawer.svelte";
import { clearToasts, getToasts } from "./toast.svelte.js";

afterEach(() => {
  cleanup();
  clearToasts();
  vi.restoreAllMocks();
});

it.each([1, 3])(
  "shows a recoverable photo group with %s files and refreshes after restoring it",
  async (count) => {
    const trashed = {
      id: 42,
      parent_id: 1,
      name: "quarterly-report.txt",
      kind: "file",
      current_version_id: "11111111-1111-4111-8111-111111111111",
      blob_hash: "a".repeat(64),
      size: 74,
      mime_type: "text/plain",
      revision: 3,
      created_at: "2026-07-28T12:00:00Z",
      modified_at: "2026-07-28T12:00:00Z",
      trashed_at: "2026-07-28T12:01:00Z",
      photo_asset_id: "11111111-1111-4111-8111-111111111112",
      photo_file_count: count,
    };
    const restored = {
      ...trashed,
      name: "quarterly-report (2).txt",
      revision: 4,
      modified_at: "2026-07-28T12:02:00Z",
      trashed_at: undefined,
      path: "/Reports/quarterly-report (2).txt",
    };
    let restoredOnServer = false;
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(
      async (input) =>
        new Response(
          JSON.stringify(
            String(input).startsWith("/api/v1/trash?")
              ? {
                  items: restoredOnServer ? [] : [trashed],
                  total: restoredOnServer ? 0 : 1,
                  limit: 1000,
                  offset: 0,
                }
              : ((restoredOnServer = true), restored),
          ),
          { status: 200, headers: { "Content-Type": "application/json" } },
        ),
    );
    const onrestored = vi.fn();

    render(TrashDrawer, {
      session: "short-lived",
      onclose: vi.fn(),
      onrestored,
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("quarterly-report.txt")).toBeTruthy();
    expect(screen.getByText(`Photo · ${count} trashed file${count === 1 ? "" : "s"}`)).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Restore" }));
    expect(screen.queryByRole("dialog", { name: /^Restore/ })).toBeNull();

    await waitFor(() =>
      expect(onrestored).toHaveBeenCalledWith(
        restored,
        count === 1 ? "Restored to /Reports" : `Restored ${count} files`,
      ),
    );
    expect(await screen.findByText("Trash is empty")).toBeTruthy();
    const [, request] = fetchMock.mock.calls[1] ?? [];
    expect(new Headers(request?.headers).get("If-Match")).toBe("3");
  },
);

it("drops stale trash rows when the refresh after a successful restore fails", async () => {
  const photo = {
    id: 42,
    name: "capture.jpg",
    kind: "file",
    size: 74,
    revision: 3,
    created_at: "2026-07-28T12:00:00Z",
    modified_at: "2026-07-28T12:00:00Z",
    trashed_at: "2026-07-28T12:01:00Z",
    photo_asset_id: "11111111-1111-4111-8111-111111111112",
    photo_file_count: 3,
  };
  const folder = {
    ...photo,
    id: 43,
    name: "Companions",
    kind: "dir",
    photo_asset_id: undefined,
    photo_file_count: undefined,
  };
  const receipt = { ...photo, revision: 4, trashed_at: undefined, path: "/Photos/capture.jpg" };
  const remaining = { ...folder, id: 44, name: "Remaining folder" };
  const listing = (items: (typeof folder)[]) =>
    new Response(JSON.stringify({ items, total: items.length, limit: 1000, offset: 0 }));
  let finishRead!: (response: Response) => void;
  vi.spyOn(globalThis, "fetch")
    .mockResolvedValueOnce(
      new Response(JSON.stringify({ items: [photo, folder], total: 2, limit: 1000, offset: 0 })),
    )
    .mockResolvedValueOnce(new Response(JSON.stringify(receipt)))
    .mockImplementationOnce(() => new Promise((resolve) => (finishRead = resolve)))
    .mockResolvedValueOnce(listing([remaining]));
  const onrestored = vi.fn();
  render(TrashDrawer, {
    session: "short-lived",
    onclose: vi.fn(),
    onrestored,
    onauthfailure: vi.fn(),
  });
  expect(await screen.findByText("capture.jpg")).toBeTruthy();
  expect(screen.getByText("Companions")).toBeTruthy();
  await fireEvent.click(screen.getAllByRole("button", { name: "Restore" })[0]);
  await waitFor(() => expect(onrestored).toHaveBeenCalledWith(receipt, "Restored 3 files"));
  expect(screen.queryByRole("dialog", { name: /^Restore/ })).toBeNull();
  await waitFor(() => expect(screen.queryByText("capture.jpg")).toBeNull());
  expect(screen.queryByText("Companions")).toBeNull();
  expect(screen.queryByRole("button", { name: "Restore" })).toBeNull();
  finishRead(
    new Response(JSON.stringify({ detail: "Trash refresh unavailable" }), { status: 503 }),
  );
  expect(await screen.findByRole("alert")).toHaveProperty(
    "textContent",
    "Trash refresh unavailable",
  );
  expect(screen.queryByRole("button", { name: "Restore" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Try again" }));
  expect(await screen.findByText("Remaining folder")).toBeTruthy();
  expect(screen.getAllByRole("button", { name: "Restore" })).toHaveLength(1);
  expect(screen.queryByText("capture.jpg")).toBeNull();
  expect(screen.queryByText("Companions")).toBeNull();
  expect(onrestored).toHaveBeenCalledTimes(1);
});

it("keeps stale restore authority visible for a fresh decision", async () => {
  const trashed = {
    id: 42,
    name: "quarterly-report.txt",
    kind: "file",
    size: 74,
    revision: 3,
    created_at: "2026-07-28T12:00:00Z",
    modified_at: "2026-07-28T12:00:00Z",
    trashed_at: "2026-07-28T12:01:00Z",
  };
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    if (String(input).startsWith("/api/v1/trash?")) {
      return new Response(JSON.stringify({ items: [trashed], total: 1, limit: 1000, offset: 0 }), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }
    return new Response(
      JSON.stringify({
        status: 412,
        code: "stale_revision",
        detail: "node changed; refresh trash before restoring it",
      }),
      {
        status: 412,
        headers: { "Content-Type": "application/problem+json" },
      },
    );
  });

  render(TrashDrawer, {
    session: "short-lived",
    onclose: vi.fn(),
    onrestored: vi.fn(),
    onauthfailure: vi.fn(),
  });

  await fireEvent.click(await screen.findByRole("button", { name: "Restore" }));
  await waitFor(() =>
    expect(getToasts().map((toast) => toast.message)).toEqual([
      "node changed; refresh trash before restoring it",
    ]),
  );
  expect(getToasts()[0]!.tone).toBe("danger");
  expect(screen.getByText("quarterly-report.txt")).toBeTruthy();
  expect((screen.getByRole("button", { name: "Restore" }) as HTMLButtonElement).disabled).toBe(
    false,
  );
});
