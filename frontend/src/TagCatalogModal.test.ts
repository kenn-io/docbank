import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/svelte";
import TagCatalogModal from "./TagCatalogModal.svelte";

beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const tax = {
  id: "11111111-1111-4111-8111-111111111111",
  name: "tax",
  revision: 2,
  assignment_count: 3,
};

it("creates, renames, and deliberately deletes stable tag definitions", async () => {
  const reviewedID = "22222222-2222-4222-8222-222222222222";
  const onchanged = vi.fn();
  const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(
    async (_path, request) => {
      if (request?.method === "POST") {
        return new Response(
          JSON.stringify({
            id: reviewedID,
            name: "reviewed",
            revision: 1,
            assignment_count: 0,
          }),
          { status: 201, headers: { "Content-Type": "application/json" } },
        );
      }
      if (request?.method === "PATCH") {
        return new Response(
          JSON.stringify({
            ...tax,
            name: "tax filing",
            revision: 3,
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      }
      return new Response(
        JSON.stringify({
          tag: { ...tax, name: "tax filing", revision: 3 },
          removed_assignments: 3,
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    },
  );

  render(TagCatalogModal, {
    session: "short-lived",
    catalog: [tax],
    catalogTotal: 1,
    disabled: false,
    onclose: vi.fn(),
    onchanged,
    onauthfailure: vi.fn(),
  });

  await fireEvent.input(screen.getByRole("textbox", { name: "New tag name" }), {
    target: { value: "reviewed" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Create" }));
  expect(await screen.findByText("Created reviewed.")).toBeTruthy();

  await fireEvent.click(screen.getByRole("button", { name: "Rename tag tax" }));
  const renameInput = screen.getByRole("textbox", { name: "Rename tax" });
  await fireEvent.input(renameInput, { target: { value: "tax filing" } });
  await fireEvent.click(
    screen.getByRole("button", { name: "Save renamed tag tax" }),
  );
  expect(await screen.findByText("Renamed tag to tax filing.")).toBeTruthy();

  await fireEvent.click(
    screen.getByRole("button", { name: "Delete tag tax filing" }),
  );
  expect(
    screen.getByText(
      "This permanently removes the tag definition and every assignment that currently uses it. Documents and their stored content are not deleted.",
    ),
  ).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Delete tag" }));
  expect(
    await screen.findByText(
      "Deleted tax filing and removed 3 assignments.",
    ),
  ).toBeTruthy();

  expect(fetchMock).toHaveBeenCalledTimes(3);
  expect(
    new Headers(fetchMock.mock.calls[1]?.[1]?.headers).get("If-Match"),
  ).toBe("2");
  expect(
    new Headers(fetchMock.mock.calls[2]?.[1]?.headers).get("If-Match"),
  ).toBe("3");
  expect(onchanged).toHaveBeenNthCalledWith(
    1,
    expect.objectContaining({
      kind: "created",
      tag: expect.objectContaining({ id: reviewedID }),
    }),
  );
  expect(onchanged).toHaveBeenNthCalledWith(
    2,
    expect.objectContaining({
      kind: "renamed",
      tag: expect.objectContaining({ name: "tax filing" }),
    }),
  );
  expect(onchanged).toHaveBeenNthCalledWith(
    3,
    expect.objectContaining({
      kind: "deleted",
      removedAssignments: 3,
    }),
  );
});

it("keeps a stale rename visible without losing the inspected definition", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(
    new Response(
      JSON.stringify({
        status: 412,
        code: "stale_revision",
        detail: "tag changed; refresh before renaming",
      }),
      {
        status: 412,
        headers: { "Content-Type": "application/problem+json" },
      },
    ),
  );

  render(TagCatalogModal, {
    session: "short-lived",
    catalog: [tax],
    catalogTotal: 1,
    disabled: false,
    onclose: vi.fn(),
    onchanged: vi.fn(),
    onauthfailure: vi.fn(),
  });

  await fireEvent.click(screen.getByRole("button", { name: "Rename tag tax" }));
  await fireEvent.input(screen.getByRole("textbox", { name: "Rename tax" }), {
    target: { value: "tax filing" },
  });
  await fireEvent.click(
    screen.getByRole("button", { name: "Save renamed tag tax" }),
  );

  expect(
    await screen.findByText("tag changed; refresh before renaming"),
  ).toBeTruthy();
  await waitFor(() =>
    expect(screen.getByRole("textbox", { name: "Rename tax" })).toBeTruthy(),
  );
});
