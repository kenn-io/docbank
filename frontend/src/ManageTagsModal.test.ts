import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
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
  name: "matter/acme/reviewed",
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
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (_input, request) => {
    const assigning = request?.method === "PUT";
    return new Response(
      JSON.stringify({
        tag: assigning
          ? { ...reviewed, revision: 2, assignment_count: 3 }
          : { ...tax, revision: 3, assignment_count: 0 },
        node: {
          ...node,
          revision: assigning ? 4 : 5,
          modified_at: assigning ? "2026-07-28T12:01:00Z" : "2026-07-28T12:02:00Z",
        },
        changed: true,
      }),
      { status: 200, headers: { "Content-Type": "application/json" } },
    );
  });

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

  await fireEvent.input(screen.getByRole("combobox", { name: "Tag to assign" }), {
    target: { value: "reviewed" },
  });
  expect(screen.getByText("matter/acme")).toBeTruthy();
  const reviewedOption = screen.getByRole("option", {
    name: "matter/acme/reviewed (2)",
  });
  expect(reviewedOption.textContent).not.toContain("matter/acme/reviewed");
  await fireEvent.click(reviewedOption);
  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
  await screen.findByRole("button", { name: "Remove tag matter/acme/reviewed" });
  expect(screen.queryByRole("status", { name: "Loading" })).toBeNull();
  const assignedName = screen.getByText("matter/acme/reviewed");
  expect(
    assignedName.parentElement?.querySelector<HTMLElement>("[aria-hidden='true']")?.textContent,
  ).toBe("reviewed");

  await fireEvent.click(screen.getByRole("button", { name: "Remove tag tax" }));
  await waitFor(() => expect(screen.queryByRole("button", { name: "Remove tag tax" })).toBeNull());

  expect(fetchMock).toHaveBeenCalledTimes(2);
  expect(new Headers(fetchMock.mock.calls[0]?.[1]?.headers).get("If-Match")).toBe("3");
  expect(new Headers(fetchMock.mock.calls[1]?.[1]?.headers).get("If-Match")).toBe("4");
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

  await fireEvent.input(screen.getByRole("combobox", { name: "Tag to assign" }), {
    target: { value: "reviewed" },
  });
  await fireEvent.click(screen.getByRole("option", { name: "matter/acme/reviewed (2)" }));

  expect(await screen.findByText("node changed; refresh before assigning tags")).toBeTruthy();
  await waitFor(() =>
    expect(
      screen.getByRole("dialog", { name: "Manage tags for quarterly-report.txt" }),
    ).toBeTruthy(),
  );
});

it("ignores a pending authorization failure after the modal is destroyed", async () => {
  let resolveResponse!: (response: Response) => void;
  let markFetchStarted!: () => void;
  const response = new Promise<Response>((resolve) => {
    resolveResponse = resolve;
  });
  const fetchStarted = new Promise<void>((resolve) => {
    markFetchStarted = resolve;
  });
  vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
    markFetchStarted();
    return response;
  });
  const onauthfailure = vi.fn();

  const mounted = render(ManageTagsModal, {
    session: "old-tab",
    node,
    catalog: [tax],
    catalogTotal: 1,
    assignedTags: [tax],
    assignedTotal: 1,
    disabled: false,
    onclose: vi.fn(),
    onchanged: vi.fn(),
    onauthfailure,
  });

  await fireEvent.click(screen.getByRole("button", { name: "Remove tag tax" }));
  await fetchStarted;
  mounted.unmount();
  resolveResponse(
    new Response(JSON.stringify({ detail: "session expired" }), {
      status: 401,
      headers: { "Content-Type": "application/problem+json" },
    }),
  );
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(onauthfailure).not.toHaveBeenCalled();
});

it("creates and assigns the first tag without a second button", async () => {
  const onchanged = vi.fn();
  const created = { ...reviewed, name: "Review", revision: 1, assignment_count: 0 };
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async (url, request) => {
    if (url === "/api/v1/tags" && request?.method === "POST") return Response.json(created);
    if (url === `/api/v1/nodes/${node.id}/tags/${created.id}` && request?.method === "PUT")
      return Response.json({
        tag: { ...created, revision: 2, assignment_count: 1 },
        node: { ...node, revision: 4 },
        changed: true,
      });
    throw new Error(`Unexpected request ${String(url)}`);
  });
  render(ManageTagsModal, {
    session: "session",
    node,
    catalog: [],
    catalogTotal: 0,
    assignedTags: [],
    assignedTotal: 0,
    disabled: false,
    onclose: vi.fn(),
    onchanged,
    onauthfailure: vi.fn(),
  });
  const input = screen.getByRole("combobox", { name: "Tag to assign" });
  await fireEvent.input(input, { target: { value: "Review" } });
  await fireEvent.keyDown(input, { key: "Enter" });
  expect(await screen.findByRole("button", { name: "Remove tag Review" })).toBeTruthy();
  expect(onchanged).toHaveBeenCalledWith(
    expect.objectContaining({ node: expect.objectContaining({ revision: 4 }) }),
    true,
  );
  expect(new Headers(fetchMock.mock.calls[1]?.[1]?.headers).get("If-Match")).toBe("3");
});

it("recognizes an assigned tag outside the loaded catalog", async () => {
  render(ManageTagsModal, {
    session: "session",
    node,
    catalog: [],
    catalogTotal: 1001,
    assignedTags: [tax],
    assignedTotal: 1,
    disabled: false,
    onclose: vi.fn(),
    onchanged: vi.fn(),
    onauthfailure: vi.fn(),
  });
  await fireEvent.input(screen.getByRole("combobox", { name: "Tag to assign" }), {
    target: { value: "tax" },
  });
  expect(screen.queryByRole("option")).toBeNull();
});
