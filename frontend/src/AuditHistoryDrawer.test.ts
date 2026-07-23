import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/svelte";
import AuditHistoryDrawer from "./AuditHistoryDrawer.svelte";
import type { AuditEvent, AuditEventPage, Node } from "./api.js";

const node: Node = {
  id: 42,
  name: "return.pdf",
  kind: "file",
  current_version_id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
  blob_hash: "b".repeat(64),
  size: 128,
  mime_type: "application/pdf",
  revision: 3,
  created_at: "2026-01-02T03:04:05Z",
  modified_at: "2026-07-23T12:00:00Z",
};

function event(overrides: Partial<AuditEvent>): AuditEvent {
  return {
    id: "c".repeat(64),
    operation_id: "11111111-1111-4111-8111-111111111111",
    operation_sequence: 3,
    ordinal: 0,
    node_id: 42,
    kind: "node_path",
    scope_id: "22222222-2222-4222-8222-222222222222",
    recorded_at: "2026-07-23T12:00:00Z",
    origin: "api",
    prior_node_revision: 2,
    resulting_node_revision: 3,
    old_path: { path: "/Taxes/draft.pdf", state: "live" },
    new_path: { path: "/Taxes/return.pdf", state: "live" },
    ...overrides,
  };
}

function page(
  items: AuditEvent[],
  nextCursor = "",
  cursor = "",
): AuditEventPage {
  return {
    node,
    path: "/Taxes/return.pdf",
    items,
    total: 2,
    limit: 50,
    cursor,
    next_cursor: nextCursor,
  };
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("audited history drawer", () => {
  it("uses the authoritative path returned with history", async () => {
    const response = page([event({})]);
    response.path = "/Filed/return.pdf";
    response.node = { ...node, parent_id: 99, revision: 4 };
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      new Response(JSON.stringify(response), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    render(AuditHistoryDrawer, {
      session: "short-lived",
      node,
      path: "/Taxes/return.pdf",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("/Filed/return.pdf")).toBeTruthy();
  });

  it("does not present a failed initial load as empty history", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(
      new Response("history unavailable", { status: 500 }),
    );

    render(AuditHistoryDrawer, {
      session: "short-lived",
      node,
      path: "/Taxes/return.pdf",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.queryByText("No events on this page")).toBeNull();
  });

  it("paginates immutable events and exposes complete typed details", async () => {
    const pathEvent = event({});
    const tagEvent = event({
      id: "d".repeat(64),
      operation_sequence: 2,
      kind: "tag_rename",
      prior_node_revision: 1,
      resulting_node_revision: 2,
      old_path: undefined,
      new_path: undefined,
      attachment: {
        kind: "tag_definition",
        identity: { tag_id: "33333333-3333-4333-8333-333333333333" },
        before: { tag_name: "draft" },
        after: { tag_name: "filed" },
      },
    });
    const fetchMock = vi
      .spyOn(globalThis, "fetch")
      .mockImplementationOnce(async () =>
        new Response(JSON.stringify(page([pathEvent], "older")), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      )
      .mockImplementationOnce(async () =>
        new Response(JSON.stringify(page([tagEvent], "", "older")), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    const close = vi.fn();

    render(AuditHistoryDrawer, {
      session: "short-lived",
      node,
      path: "/Taxes/return.pdf",
      onclose: close,
      onauthfailure: vi.fn(),
    });

    expect(await screen.findByText("/Taxes/draft.pdf → /Taxes/return.pdf")).toBeTruthy();
    expect(screen.getByText(pathEvent.id)).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Load older events" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));

    const tagHeading = await screen.findByText("Tag renamed");
    const tagButton = tagHeading.closest("button");
    expect(tagButton).toBeTruthy();
    await fireEvent.click(tagButton!);

    expect(screen.getByText("draft → filed")).toBeTruthy();
    expect(screen.getByText("33333333-3333-4333-8333-333333333333")).toBeTruthy();
    expect(screen.getByText("Before")).toBeTruthy();
    expect(screen.getByText("After")).toBeTruthy();

    await fireEvent.click(screen.getByRole("button", { name: "Close audit history" }));
    expect(close).toHaveBeenCalledOnce();
  });
});
