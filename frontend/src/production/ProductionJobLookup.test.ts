import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ProductionJobLookup from "./ProductionJobLookup.svelte";
import type { ProductionDraft, ProductionSet } from "./api.js";

const set: ProductionSet = { id: "11111111-1111-4111-8111-111111111111", name: "Synthetic finalized",
  creator: "synthetic", created_at: "2026-09-27T00:00:00Z", head_revision: 2 };
const draft: ProductionDraft = { set_id: set.id, revision: 2, etag: 5, state: "finalized",
  membership_sealed: true, member_hash: "a".repeat(64) };

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

it("starts an exact finalized job only after confirmation and displays its status", async () => {
  const posts: { job_id: string; operation_id: string }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith(`/sets/${set.id}`)) return Response.json(set);
    if (url.endsWith(`/sets/${set.id}/revisions/2`)) return Response.json(draft);
    if (url.endsWith(`/sets/${set.id}/revisions/2/jobs`) && init?.method === "POST") {
      const body = JSON.parse(String(init.body)); posts.push(body);
      return Response.json({ job_id: body.job_id, set_id: set.id, revision: 2,
        state: "queued", revision_sha256: "b".repeat(64) });
    }
    throw new Error(`unexpected ${url}`);
  });
  render(ProductionJobLookup, { session: "synthetic", setID: set.id, set, draft,
    onauthfailure: vi.fn() });
  const start = screen.getByRole("button", { name: "Start production job" });
  expect((start as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.click(screen.getByRole("checkbox", { name: /ready to run this finalized revision/i }));
  await fireEvent.click(start);
  await waitFor(() => expect(posts).toHaveLength(1));
  expect(posts[0].job_id).toMatch(/^[0-9a-f-]{36}$/);
  expect(posts[0].operation_id).toMatch(/^[0-9a-f-]{36}$/);
  expect(await screen.findByRole("region", { name: `Status for production job ${posts[0].job_id}` })).toBeTruthy();
});

it("reuses both identities when the admission response is lost", async () => {
  const posts: { job_id: string; operation_id: string }[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith(`/sets/${set.id}`)) return Response.json(set);
    if (url.endsWith(`/sets/${set.id}/revisions/2`)) return Response.json(draft);
    if (url.endsWith(`/sets/${set.id}/revisions/2/jobs`) && init?.method === "POST") {
      const body = JSON.parse(String(init.body)); posts.push(body);
      if (posts.length === 1) throw new TypeError("synthetic lost response");
      return Response.json({ job_id: body.job_id, set_id: set.id, revision: 2,
        state: "queued", revision_sha256: "b".repeat(64) });
    }
    throw new Error(`unexpected ${url}`);
  });
  render(ProductionJobLookup, { session: "synthetic", setID: set.id, set, draft,
    onauthfailure: vi.fn() });
  await fireEvent.click(screen.getByRole("checkbox", { name: /ready to run this finalized revision/i }));
  await fireEvent.click(screen.getByRole("button", { name: "Start production job" }));
  expect((await screen.findByRole("alert")).textContent).toContain("synthetic lost response");
  await fireEvent.click(screen.getByRole("button", { name: "Retry start" }));
  await waitFor(() => expect(posts).toHaveLength(2));
  expect(posts[1]).toEqual(posts[0]);
});

it("does not rebind an uncertain job attempt to a changed draft", async () => {
  let posts = 0;
  let currentDraft = draft;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.endsWith(`/sets/${set.id}`)) return Response.json(set);
    if (url.endsWith(`/sets/${set.id}/revisions/2`)) return Response.json(currentDraft);
    if (url.endsWith(`/sets/${set.id}/revisions/2/jobs`) && init?.method === "POST") {
      posts++;
      throw new TypeError("synthetic lost response");
    }
    throw new Error(`unexpected ${url}`);
  });
  const props = { session: "synthetic", setID: set.id, set, draft, onauthfailure: vi.fn() };
  const view = render(ProductionJobLookup, props);
  await fireEvent.click(screen.getByRole("checkbox", { name: /ready to run this finalized revision/i }));
  await fireEvent.click(screen.getByRole("button", { name: "Start production job" }));
  await screen.findByRole("alert");
  currentDraft = { ...draft, etag: 6 };
  await view.rerender({ ...props, draft: currentDraft });
  await fireEvent.click(screen.getByRole("button", { name: "Retry start" }));
  await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/revision changed/i));
  expect(posts).toBe(1);
  await view.rerender({ ...props, draft: { ...currentDraft, state: "draft" } });
  expect(screen.getByRole("button", { name: "Check job status" })).toBeTruthy();
});
