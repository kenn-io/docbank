import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/svelte";
import ManageTagsModal from "./ManageTagsModal.svelte";

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
  Object.defineProperty(Element.prototype, "scrollIntoView", {
    configurable: true,
    value: vi.fn(),
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
  vi.restoreAllMocks();
});

const tax = {
  id: "11111111-1111-4111-8111-111111111111",
  name: "tax",
  revision: 2,
  assignment_count: 1,
};
const reviewed = {
  id: "22222222-2222-4222-8222-222222222222",
  name: "reviewed",
  revision: 1,
  assignment_count: 2,
};
const node = {
  id: 42,
  parent_id: 2,
  name: "quarterly-report.txt",
  kind: "file" as const,
  size: 74,
  revision: 3,
  created_at: "2026-07-28T12:00:00Z",
  modified_at: "2026-07-28T12:00:00Z",
  path: "/Reports/quarterly-report.txt",
};

it("adds and removes existing tags under consecutive node revisions", async () => {
  const onchanged = vi.fn();
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(
    async (_input, request) => {
      const assigning = request?.method === "PUT";
      return new Response(
        JSON.stringify({
          tag: assigning
            ? { ...reviewed, revision: 2, assignment_count: 3 }
            : { ...tax, revision: 3, assignment_count: 0 },
          node: {
            ...node,
            revision: assigning ? 4 : 5,
            modified_at: assigning
              ? "2026-07-28T12:01:00Z"
              : "2026-07-28T12:02:00Z",
          },
          changed: true,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    },
  );

  render(ManageTagsModal, {
    session: "short-lived",
    node,
    catalog: [reviewed, tax],
    catalogTotal: 2,
    assignedTags: [tax],
    assignedTotal: 1,
    disabled: false,
    onclose: vi.fn(),
    onchanged,
    onauthfailure: vi.fn(),
  });

  await fireEvent.click(
    screen.getByRole("combobox", { name: "Tag to assign: Choose a tag…" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "reviewed (2)" }));
  await fireEvent.click(screen.getByRole("button", { name: "Add tag" }));
  expect(await screen.findByText("Added reviewed.")).toBeTruthy();

  await fireEvent.click(screen.getByRole("button", { name: "Remove tag tax" }));
  expect(await screen.findByText("Removed tax.")).toBeTruthy();

  expect(fetchMock).toHaveBeenCalledTimes(2);
  expect(new Headers(fetchMock.mock.calls[0]?.[1]?.headers).get("If-Match")).toBe(
    "3",
  );
  expect(new Headers(fetchMock.mock.calls[1]?.[1]?.headers).get("If-Match")).toBe(
    "4",
  );
  expect(onchanged).toHaveBeenNthCalledWith(
    1,
    expect.objectContaining({
      tag: expect.objectContaining({ id: reviewed.id }),
      node: expect.objectContaining({ revision: 4 }),
    }),
    true,
  );
  expect(onchanged).toHaveBeenNthCalledWith(
    2,
    expect.objectContaining({
      tag: expect.objectContaining({ id: tax.id }),
      node: expect.objectContaining({ revision: 5 }),
    }),
    false,
  );
});

it("keeps a stale assignment decision visible for refresh", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        status: 412,
        code: "stale_revision",
        detail: "node changed; refresh before assigning tags",
      }),
      {
        status: 412,
        headers: { "Content-Type": "application/problem+json" },
      },
    ),
  );

  render(ManageTagsModal, {
    session: "short-lived",
    node,
    catalog: [reviewed],
    catalogTotal: 1,
    assignedTags: [],
    assignedTotal: 0,
    disabled: false,
    onclose: vi.fn(),
    onchanged: vi.fn(),
    onauthfailure: vi.fn(),
  });

  await fireEvent.click(
    screen.getByRole("combobox", { name: "Tag to assign: Choose a tag…" }),
  );
  await fireEvent.click(screen.getByRole("option", { name: "reviewed (2)" }));
  await fireEvent.click(screen.getByRole("button", { name: "Add tag" }));

  expect(
    await screen.findByText("node changed; refresh before assigning tags"),
  ).toBeTruthy();
  await waitFor(() =>
    expect(
      screen.getByRole("dialog", { name: "Manage tags for quarterly-report.txt" }),
    ).toBeTruthy(),
  );
});
