import { afterEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/svelte";
import CollectionsDrawer from "./CollectionsDrawer.svelte";

const collectionID = "11111111-1111-4111-8111-111111111111";
const versionID = "22222222-2222-4222-8222-222222222222";
const collection = {
  id: collectionID,
  source_kind: "cli",
  source_description: "/synthetic/drop/discovery",
  started_at: "2026-09-09T14:30:00Z",
  file_count: 2,
  total_bytes: 4096,
  label: "Discovery batch",
  label_revision: 3,
  label_updated_at: "2026-09-09T14:31:00Z",
};
const member = {
  id: 42,
  parent_id: 7,
  name: "report.txt",
  kind: "file" as const,
  path: "/Archive/report.txt",
  current_version_id: versionID,
  blob_hash: "a".repeat(64),
  size: 2048,
  revision: 5,
  created_at: "2026-09-09T14:30:00Z",
  modified_at: "2026-09-09T14:30:00Z",
};
const members = [
  member,
  {
    ...member,
    id: 43,
    name: "notes.txt",
    path: "/Archive/notes.txt",
    current_version_id: "33333333-3333-4333-8333-333333333333",
    blob_hash: "b".repeat(64),
    size: 1024,
  },
];

function json(value: unknown, etag?: string): Response {
  return new Response(JSON.stringify(value), {
    headers: {
      "Content-Type": "application/json",
      ...(etag ? { ETag: etag } : {}),
    },
  });
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("collections drawer", () => {
  it("shows supported collection facts and opens an exact direct member", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
      const url = String(input);
      if (url === "/api/v1/collections?limit=100&offset=0") {
        return json({ items: [collection], total: 1, limit: 100, offset: 0 });
      }
      if (
        url ===
        `/api/v1/collections/${collectionID}/members?limit=100&offset=0`
      ) {
        return json({
          collection,
          items: members,
          total: 2,
          limit: 100,
          offset: 0,
        });
      }
      if (url === `/api/v1/collections/${collectionID}/label`) {
        return json(
          {
            ingest_id: collectionID,
            label: collection.label,
            revision: collection.label_revision,
            updated_at: collection.label_updated_at,
          },
          '"3"',
        );
      }
      throw new Error(`unexpected request: ${url}`);
    });
    const openMember = vi.fn();

    render(CollectionsDrawer, {
      session: "short-lived",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
      onopenmember: openMember,
    });

    const card = await screen.findByRole("button", {
      name: "Browse collection Discovery batch",
    });
    expect(within(card).getByText("2 documents")).toBeTruthy();
    expect(within(card).getByText("4.00 KiB")).toBeTruthy();
    expect(within(card).getByText("/synthetic/drop/discovery")).toBeTruthy();
    expect(within(card).getByText(/Imported /)).toBeTruthy();
    expect(screen.queryByText(/successful/i)).toBeNull();

    await fireEvent.click(card);
    expect(await screen.findByText("Documents in this collection")).toBeTruthy();
    expect(await screen.findByText(/^2 direct members/)).toBeTruthy();
    const open = await screen.findByRole("button", {
      name: "Open document /Archive/report.txt",
    });
    await fireEvent.click(open);
    expect(openMember.mock.calls[0]?.[0]).toEqual(member);
  });

  it("keeps a conflicting label draft until an explicit reload and clear", async () => {
    const requests: RequestInit[] = [];
    let labelReads = 0;
    let labelWrites = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init = {}) => {
      const url = String(input);
      if (url === "/api/v1/collections?limit=100&offset=0") {
        return json({ items: [collection], total: 1, limit: 100, offset: 0 });
      }
      if (url.endsWith("/members?limit=100&offset=0")) {
        return json({ collection, items: members, total: 2, limit: 100, offset: 0 });
      }
      if (url.endsWith("/label") && init.method === "PUT") {
        requests.push(init);
        labelWrites += 1;
        if (labelWrites === 1) {
          return new Response(
            JSON.stringify({ code: "stale_revision", detail: "Label changed elsewhere" }),
            { status: 412, headers: { "Content-Type": "application/json" } },
          );
        }
        return json(
          {
            ingest_id: collectionID,
            label: null,
            revision: 5,
            updated_at: "2026-09-09T15:00:00Z",
          },
          '"5"',
        );
      }
      if (url.endsWith("/label")) {
        labelReads += 1;
        return labelReads === 1
          ? json(
              {
                ingest_id: collectionID,
                label: "Discovery batch",
                revision: 3,
                updated_at: collection.label_updated_at,
              },
              '"3"',
            )
          : json(
              {
                ingest_id: collectionID,
                label: "Externally labeled",
                revision: 4,
                updated_at: "2026-09-09T14:45:00Z",
              },
              '"4"',
            );
      }
      throw new Error(`unexpected request: ${url}`);
    });

    render(CollectionsDrawer, {
      session: "short-lived",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
      onopenmember: vi.fn(),
    });
    await fireEvent.click(
      await screen.findByRole("button", { name: "Browse collection Discovery batch" }),
    );
    const input = await screen.findByRole("textbox", { name: "Collection label" });
    await fireEvent.input(input, { target: { value: "Keep this draft" } });
    await fireEvent.click(screen.getByRole("button", { name: "Save label" }));

    expect((await screen.findByRole("alert")).textContent).toContain("Label changed elsewhere");
    expect((input as HTMLInputElement).value).toBe("Keep this draft");
    expect(labelWrites).toBe(1);

    await fireEvent.click(screen.getByRole("button", { name: "Reload label" }));
    expect(await screen.findByDisplayValue("Externally labeled")).toBeTruthy();
    await fireEvent.click(screen.getByRole("button", { name: "Clear label" }));
    expect(
      await screen.findByRole("button", {
        name: `Browse collection Unlabeled import ${collectionID.slice(0, 8)}`,
      }),
    ).toBeTruthy();
    expect(new Headers(requests[1]?.headers).get("If-Match")).toBe('"4"');
    expect(JSON.parse(String(requests[1]?.body))).toEqual({ label: null });
  });

  it("removes the previous session's collection while the new session loads", async () => {
    let resolveFresh!: (response: Response) => void;
    const fresh = new Promise<Response>((resolve) => {
      resolveFresh = resolve;
    });
    vi.spyOn(globalThis, "fetch").mockImplementation(async (_input, init) => {
      const session = new Headers(init?.headers).get("X-Docbank-Web-Session");
      if (session === "old-session") {
        return json({ items: [collection], total: 1, limit: 100, offset: 0 });
      }
      if (session === "fresh-session") return fresh;
      throw new Error(`unexpected session: ${session}`);
    });

    const view = render(CollectionsDrawer, {
      session: "old-session",
      onclose: vi.fn(),
      onauthfailure: vi.fn(),
      onopenmember: vi.fn(),
    });
    await screen.findByRole("button", { name: "Browse collection Discovery batch" });

    await view.rerender({ session: "fresh-session" });
    await waitFor(() => {
      expect(screen.queryByRole("button", { name: "Browse collection Discovery batch" })).toBeNull();
    });
    expect(screen.getByText("Loading import collections…")).toBeTruthy();

    resolveFresh(json({ items: [], total: 0, limit: 100, offset: 0 }));
    expect(await screen.findByText("No import collections")).toBeTruthy();
  });
});
