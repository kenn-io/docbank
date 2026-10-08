import { afterEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/svelte";
import TrashDrawer from "./TrashDrawer.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it.each([1, 3])("shows a recoverable photo group with %s files and refreshes after restoring it", async count => {
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
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) =>
    new Response(
      JSON.stringify(
        String(input).startsWith("/api/v1/trash?")
          ? { items: restoredOnServer ? [] : [trashed], total: restoredOnServer ? 0 : 1, limit: 1000, offset: 0 }
          : (restoredOnServer = true, restored),
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
  const dialog = screen.getByRole("dialog", {
    name: "Restore quarterly-report.txt from trash",
  });
  expect(dialog).toBeTruthy();
  expect(within(dialog).getByText(new RegExp(`recovers ${count} trashed member file${count === 1 ? "" : "s"} together`))).toBeTruthy();
  expect(
    screen.getByText(
      "Restore keeps the same stable node and retained content. It does not roll back versions or alter permanent audited history.",
    ),
  ).toBeTruthy();
  await fireEvent.click(within(dialog).getByRole("button", { name: "Restore" }));

  await waitFor(() => expect(onrestored).toHaveBeenCalledWith(restored));
  expect(
    await screen.findByText("/Reports/quarterly-report (2).txt"),
  ).toBeTruthy();
  expect(await screen.findByText("Trash is empty")).toBeTruthy();
  const [, request] = fetchMock.mock.calls[1] ?? [];
  expect(new Headers(request?.headers).get("If-Match")).toBe("3");
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
      return new Response(
        JSON.stringify({ items: [trashed], total: 1, limit: 1000, offset: 0 }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
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

  await fireEvent.click(
    await screen.findByRole("button", { name: "Restore" }),
  );
  const dialog = screen.getByRole("dialog", {
    name: "Restore quarterly-report.txt from trash",
  });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Restore" }));
  expect(
    await screen.findByText("node changed; refresh trash before restoring it"),
  ).toBeTruthy();
  expect(
    screen.getByRole("dialog", { name: "Restore quarterly-report.txt from trash" }),
  ).toBeTruthy();
});
