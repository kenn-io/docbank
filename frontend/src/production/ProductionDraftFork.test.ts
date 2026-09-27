import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ProductionDraftFork from "./ProductionDraftFork.svelte";
import type { ProductionDraft, ProductionSet } from "./api.js";

const set: ProductionSet = { id: "11111111-1111-4111-8111-111111111111", name: "Synthetic final",
  creator: "synthetic", created_at: "2026-09-27T00:00:00Z", head_revision: 2 };
const draft: ProductionDraft = { set_id: set.id, revision: 2, etag: 5, state: "finalized",
  membership_sealed: true, member_hash: "a".repeat(64), numbering_recipe_id: "bates-sequential-v1" };
const forked: ProductionDraft = { ...draft, revision: 3, etag: 1, state: "draft", membership_sealed: false };

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it("creates a new editable draft from the exact finalized revision after confirmation", async () => {
  const onrefresh = vi.fn();
  let command: { operation_id: string } | null = null;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${set.id}`) return Response.json(set);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return Response.json(draft);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2/fork` && init?.method === "POST") {
      command = JSON.parse(String(init.body));
      return Response.json(forked, { status: 201 });
    }
    throw new Error(`unexpected ${url}`);
  });
  render(ProductionDraftFork, { session: "synthetic", set, draft, onrefresh, onauthfailure: vi.fn() });
  const create = screen.getByRole("button", { name: "Create new draft" });
  expect(create).toHaveProperty("disabled", true);
  await fireEvent.click(screen.getByRole("checkbox", { name: /new editable draft.*finalized revision/i }));
  await fireEvent.click(create);
  await waitFor(() => expect(onrefresh).toHaveBeenCalledOnce());
  expect(command!.operation_id).toMatch(/^[0-9a-f-]{36}$/);
  expect(screen.getByText("Created draft revision 3")).toBeTruthy();
});

it("retries a lost fork response with the same operation ID", async () => {
  const commands: unknown[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === `/api/v1/productions/sets/${set.id}`) return Response.json(set);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2`) return Response.json(draft);
    if (url === `/api/v1/productions/sets/${set.id}/revisions/2/fork` && init?.method === "POST") {
      commands.push(JSON.parse(String(init.body)));
      if (commands.length === 1) throw new TypeError("synthetic lost response");
      return Response.json(forked, { status: 201 });
    }
    throw new Error(`unexpected ${url}`);
  });
  render(ProductionDraftFork, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(screen.getByRole("checkbox", { name: /new editable draft.*finalized revision/i }));
  await fireEvent.click(screen.getByRole("button", { name: "Create new draft" }));
  expect((await screen.findByRole("alert")).textContent).toContain("synthetic lost response");
  await fireEvent.click(screen.getByRole("button", { name: "Retry new draft" }));
  await waitFor(() => expect(commands).toHaveLength(2));
  expect(commands[1]).toEqual(commands[0]);
});

it("asks for a refresh when the source is no longer the current head", async () => {
  const requests: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async input => {
    const url = String(input); requests.push(url);
    if (url === `/api/v1/productions/sets/${set.id}`) return Response.json({ ...set, head_revision: 3 });
    throw new Error(`unexpected ${url}`);
  });
  render(ProductionDraftFork, { session: "synthetic", set, draft, onrefresh: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(screen.getByRole("checkbox", { name: /new editable draft.*finalized revision/i }));
  await fireEvent.click(screen.getByRole("button", { name: "Create new draft" }));
  expect((await screen.findByRole("alert")).textContent).toContain("changed");
  expect(screen.queryByRole("button", { name: "Retry new draft" })).toBeNull();
  expect(screen.getByRole("button", { name: "Refresh revision" })).toBeTruthy();
  expect(requests).not.toContain(`/api/v1/productions/sets/${set.id}/revisions/2/fork`);
});
