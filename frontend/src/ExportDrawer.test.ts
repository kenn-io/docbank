import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import ExportDrawer from "./ExportDrawer.svelte";
beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); Reflect.deleteProperty(Element.prototype, "scrollIntoView"); });
const input = { label: "Selected documents", members: [{ node_id: 1, version_id: "11111111-1111-4111-8111-111111111111", sha256: "a".repeat(64), size: 12 }] };

it("shows exact source scope and original warning before any server work, with explicit role omission choices", async () => {
  const fetcher = vi.spyOn(globalThis, "fetch");
  render(ExportDrawer, { session: "s", input, open: true, onclose: vi.fn(), onauthfailure: vi.fn() });
  expect(await screen.findByText("Selected documents")).toBeTruthy();
  expect(screen.getByText(/Originals are not redacted or sanitized/)).toBeTruthy();
  expect(screen.getByRole("button", { name: "Preview export" }).hasAttribute("disabled")).toBe(false);
  expect(screen.queryByRole("button", { name: "Start reviewed export" })).toBeNull();
  await fireEvent.click(screen.getByRole("combobox", { name: /^Text export policy/ }));
  expect(screen.getByRole("option", { name: "Optional — allow unavailable" })).toBeTruthy();
  expect(fetcher).not.toHaveBeenCalled();
});

it("disables empty sources and fences work when closed", async () => {
  const close = vi.fn();
  render(ExportDrawer, { session: "s", input: { label: "Empty selection", members: [] }, open: true, onclose: close, onauthfailure: vi.fn() });
  expect((await screen.findByRole("button", { name: "Preview export" })).hasAttribute("disabled")).toBe(true);
  await fireEvent.click(screen.getByRole("button", { name: "Close export" }));
  expect(close).toHaveBeenCalledOnce();
});

it("offers body PDFs with explicit partial, attachment, duplicate and bounded packaging choices", async () => {
  render(ExportDrawer, { session: "s", input, open: true, onclose: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("combobox", { name: /^Email body PDF/ }));
  await fireEvent.click(screen.getByRole("option", { name: "Include retained body PDFs" }));
  expect(screen.getByRole("button", { name: "Find retained PDF recipes" })).toBeTruthy();
  expect(screen.getByRole("combobox", { name: /^Email attachment outputs/ })).toBeTruthy();
  expect(screen.getByRole("combobox", { name: /^Partial email export/ }).textContent).toContain("Fail if any output is unavailable");
  expect(screen.getByRole("combobox", { name: /^Duplicate outputs/ }).textContent).toContain("Preserve every occurrence");
  expect(screen.getByRole("button", { name: "Preview export" }).hasAttribute("disabled")).toBe(true);
  expect(screen.getByText(/Only qualified nested-email PDFs/)).toBeTruthy();
});

it("routes an expired browser session to the app authentication handler", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ detail: "Session ended", code: "unauthorized" }), { status: 401, headers: { "Content-Type": "application/problem+json" } }));
  const onauthfailure = vi.fn(), onclose = vi.fn();
  render(ExportDrawer, { session: "s", input, open: true, onclose, onauthfailure });
  await fireEvent.click(await screen.findByRole("button", { name: "Preview export" }));
  await waitFor(() => expect(onauthfailure).toHaveBeenCalledOnce());
  expect(onclose).toHaveBeenCalledOnce();
});

it("starts a fresh preparation after a failed source reservation", async () => {
  const ids: string[] = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (_url, init) => {
    ids.push(JSON.parse(String(init?.body)).operation_id);
    return new Response(JSON.stringify({ detail: "Source preparation failed" }), { status: 409 });
  });
  render(ExportDrawer, { session: "s", input, open: true, onclose: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(await screen.findByRole("button", { name: "Preview export" }));
  await fireEvent.click(await screen.findByRole("button", { name: "Start over" }));
  expect(screen.queryByRole("alert")).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "Preview export" }));
  await screen.findByRole("button", { name: "Start over" });
  expect(ids).toHaveLength(2);
  expect(new Set(ids).size).toBe(2);
});

it("shows empty attachment sets and sends the set selected in the drawer", async () => {
  const { exportMemberHash } = await import("./exports.js");
  const memberHash = await exportMemberHash(input.members), hash = "a".repeat(64);
  let selected: unknown;
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url), body = init?.body ? JSON.parse(String(init.body)) : undefined;
    const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
    if (path.endsWith("/sources")) return json({ id: body.operation_id, request_sha256: hash, kind: "explicit", state: "sealed", member_hash: memberHash, total: 1, source_bytes: 12, created_at: "2026-01-01T00:00:00Z", expires_at: "2099-01-01T00:00:00Z" });
    const source_id = path.split("/").at(-2);
    if (path.endsWith("/email-pdf-recipes")) return json({ source_id, member_hash: memberHash, total: 1, recipes: [{ recipe_sha256: hash, paper: "A4", renderer_version: "151", messages: 1, ambiguous: 0 }] });
    if (path.includes("/attachment-publications")) return json({ source_id, member_hash: memberHash, after: 0, next: 0, total: 2, items: ["first", "second"].map(operation_id => ({ node_id: 1, version_id: input.members[0]!.version_id, name: "empty.eml", operation_id, generation_id: hash, created_at: "2026-01-01T00:00:00Z", state: "complete", attachments: 0 })) });
    selected = body.publications;
    return new Response(JSON.stringify({ detail: "No retained PDF" }), { status: 409 });
  });
  render(ExportDrawer, { session: "s", input, open: true, onclose: vi.fn(), onauthfailure: vi.fn() });
  const choose = async (name: RegExp, option: string | RegExp) => {
    await fireEvent.click(screen.getByRole("combobox", { name }));
    await fireEvent.click(screen.getByRole("option", { name: option }));
  };
  await choose(/^Email body PDF/, "Include retained body PDFs");
  await fireEvent.click(screen.getByRole("button", { name: "Find retained PDF recipes" }));
  await screen.findByRole("combobox", { name: /^Retained PDF recipe/ });
  await choose(/^Retained PDF recipe/, /A4/);
  await choose(/^Email attachment outputs/, "Include original attachments");
  await fireEvent.click(screen.getByRole("button", { name: "Preview export" }));
  const second = await screen.findByRole("radio", { name: /second/ });
  expect(screen.getAllByRole("radio").every(r => !(r as HTMLInputElement).checked)).toBe(true);
  await fireEvent.click(second);
  await fireEvent.click(screen.getByRole("button", { name: "Preview export" }));
  await waitFor(() => expect(selected).toEqual([{ version_id: input.members[0]!.version_id, operation_id: "second" }]));
});

it("keeps export start available when the first details page fails and retries that page", async () => {
  const { exportMemberHash } = await import("./exports.js");
  const memberHash = await exportMemberHash(input.members), hash = "a".repeat(64), future = "2099-01-01T00:00:00Z";
  let source: any, plan: any, detailRequests = 0, planRequests = 0;
  const json = (value: unknown) => new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } });
  vi.spyOn(globalThis, "fetch").mockImplementation(async (url, init) => {
    const path = String(url), body = init?.body ? JSON.parse(String(init.body)) : undefined;
    if (path.endsWith("/sources")) {
      source = { id: body.operation_id, request_sha256: hash, kind: "explicit", state: "sealed", member_hash: memberHash, total: 1, source_bytes: 12, created_at: "2026-01-01T00:00:00Z", expires_at: future };
      return json(source);
    }
    if (path.endsWith("/plans")) {
      planRequests++;
      plan = { format: "docbank-bundle-v1", id: body.operation_id, vault_id: input.members[0]!.version_id, toolchain: "go1.27", source, roles: body.roles, fingerprint: hash, total: 1, role_entries: 1, role_bytes: 12, metadata_bytes: 100, created_at: source.created_at, expires_at: future, counts: { messages: 0, attachments: 0, email_pdfs: 0, attachment_pdfs: 0, pages: 0, collapsed: 0, unavailable: 1, unavailable_inventories: 0 } };
      return json(plan);
    }
    if (path.endsWith("/preview")) return json({ plan_id: plan.id, fingerprint: hash, member_hash: memberHash, total: 1, roles: [{ role: "original", available_members: 1, unavailable_members: 0, files: 1, bytes: 12 }, { role: "text", available_members: 0, unavailable_members: 1, files: 0, bytes: 0, unavailable_reason: "No retained text" }] });
    expect(path).toBe(`/api/v1/exports/plans/${plan.id}/problems?after=0`);
    if (++detailRequests === 1) return new Response(JSON.stringify({ detail: "Details temporarily unavailable" }), { status: 500 });
    return json({ plan_id: plan.id, fingerprint: hash, after: 0, next: 0, total: 1, items: [{ node_id: 1, version_id: input.members[0]!.version_id, role: "text", reason: "Text was not retained for this version." }] });
  });
  render(ExportDrawer, { session: "s", input, open: true, onclose: vi.fn(), onauthfailure: vi.fn() });
  await fireEvent.click(screen.getByRole("combobox", { name: /^Text export policy/ }));
  await fireEvent.click(screen.getByRole("option", { name: "Optional — allow unavailable" }));
  await fireEvent.click(screen.getByRole("button", { name: "Preview export" }));
  await screen.findByText("Details temporarily unavailable");
  const start = screen.getByRole("button", { name: "Start reviewed export" });
  expect(start.hasAttribute("disabled")).toBe(false);
  await fireEvent.click(screen.getByRole("button", { name: "Retry unavailable output details" }));
  await screen.findByText("text: Text was not retained for this version.");
  expect(screen.queryByRole("alert")).toBeNull();
  expect(start.hasAttribute("disabled")).toBe(false);
  expect(detailRequests).toBe(2);
  expect(planRequests).toBe(1);
});
