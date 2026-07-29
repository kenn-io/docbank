import { afterEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/svelte";
import TrashNodeModal from "./TrashNodeModal.svelte";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

it("confirms one revision-bound move to recoverable trash", async () => {
  const ontrashed = vi.fn();
  const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        id: 42,
        parent_id: 1,
        name: "quarterly-report.txt",
        kind: "file",
        size: 74,
        revision: 4,
        created_at: "2026-07-28T12:00:00Z",
        modified_at: "2026-07-28T12:00:00Z",
        trashed_at: "2026-07-28T12:01:00Z",
        path: "/Reports/quarterly-report.txt",
      }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    ),
  );

  render(TrashNodeModal, {
    session: "short-lived",
    node: {
      id: 42,
      parent_id: 2,
      name: "quarterly-report.txt",
      kind: "file",
      size: 74,
      revision: 3,
      created_at: "2026-07-28T12:00:00Z",
      modified_at: "2026-07-28T12:00:00Z",
    },
    path: "/Reports/quarterly-report.txt",
    onclose: vi.fn(),
    ontrashed,
    onauthfailure: vi.fn(),
  });

  expect(
    screen.getByText(
      "This document will leave the current tree. It remains recoverable from trash.",
    ),
  ).toBeTruthy();
  expect(
    screen.getByText(
      "This does not empty trash, reclaim stored bytes, or erase permanent audited history.",
    ),
  ).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));

  expect(fetchMock).toHaveBeenCalledOnce();
  const [, request] = fetchMock.mock.calls[0] ?? [];
  expect(new Headers(request?.headers).get("If-Match")).toBe("3");
  await waitFor(() =>
    expect(ontrashed).toHaveBeenCalledWith(
      expect.objectContaining({
        id: 42,
        revision: 4,
        trashed_at: "2026-07-28T12:01:00Z",
      }),
    ),
  );
});

it("keeps the confirmation open when the inspected revision is stale", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        status: 412,
        code: "stale_revision",
        detail: "node changed; refresh before moving it to trash",
      }),
      {
        status: 412,
        headers: { "Content-Type": "application/problem+json" },
      },
    ),
  );

  render(TrashNodeModal, {
    session: "short-lived",
    node: {
      id: 42,
      name: "quarterly-report.txt",
      kind: "file",
      size: 74,
      revision: 3,
      created_at: "2026-07-28T12:00:00Z",
      modified_at: "2026-07-28T12:00:00Z",
    },
    path: "/Reports/quarterly-report.txt",
    onclose: vi.fn(),
    ontrashed: vi.fn(),
    onauthfailure: vi.fn(),
  });

  await fireEvent.click(screen.getByRole("button", { name: "Move to trash" }));
  expect(
    await screen.findByText("node changed; refresh before moving it to trash"),
  ).toBeTruthy();
  expect(screen.getByRole("dialog", { name: "Move quarterly-report.txt to trash" }))
    .toBeTruthy();
});
