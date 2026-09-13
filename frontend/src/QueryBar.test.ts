import { createHash } from "node:crypto";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { tick } from "svelte";
import QueryBar from "./QueryBar.svelte";
import { parseQuery } from "./query.js";

const canonical = '{"filters":{"exclude_tag_ids":["11111111-1111-4111-8111-111111111111"],"size_min":2},"mode":"lexical","sort":{"direction":"desc","field":"size"},"syntax":"advanced","text":"name:manual OR collection:archive","v":1}';
const query = parseQuery(canonical);
const response = (text = canonical) => new Response(JSON.stringify({ query: JSON.parse(text),
  query_fingerprint: `sha256:${createHash("sha256").update(text).digest("hex")}`, dependencies: [] }));
beforeEach(() => vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} }));
afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });
function open() {
  const onchange = vi.fn();
  const onclose = vi.fn();
  const view = render(QueryBar, { session: "session", query, onchange, onclose, onsave: vi.fn(), onauthfailure: vi.fn() });
  return { ...view, onchange, onclose };
}

it("preserves independent facets and sort while editing the entire expression", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(response());
  const { onchange } = open();
  await screen.findByText(/Query validated/);
  expect(screen.getByLabelText("Structured facet summaries").textContent).not.toContain("paths:");
  expect((screen.getByRole("button", { name: "Run query" }) as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.input(screen.getByLabelText("Query expression"), { target: { value: "NOT tag:missing" } });
  expect(onchange).toHaveBeenLastCalledWith({ ...query, text: "NOT tag:missing" });
  expect(screen.queryByText(/Query validated/)).toBeNull();
  expect((screen.getByRole("button", { name: "Save query draft" }) as HTMLButtonElement).disabled).toBe(false);
});

it("keeps invalid and duplicate facet JSON visible without dropping constraints", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(response());
  const { onchange } = open();
  for (const raw of ['{"size_min":', '{"size_min":2,"size_min":3}']) {
    await fireEvent.input(screen.getByLabelText("Structured facets JSON"), { target: { value: raw } });
    expect((screen.getByLabelText("Structured facets JSON") as HTMLTextAreaElement).value).toBe(raw);
    expect(screen.getByRole("alert")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Save query draft" }) as HTMLButtonElement).disabled).toBe(true);
  }
  expect(onchange).not.toHaveBeenCalled();
});

it.each(["success", "invalid", "backend"])("ignores an old %s after an edit before the next debounce", async (kind) => {
  let resolve!: (value: Response) => void;
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(() => new Promise<Response>((done) => { resolve = done; }));
  open();
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
  const signal = fetch.mock.calls[0][1]?.signal;
  await fireEvent.input(screen.getByLabelText("Query expression"), { target: { value: "new expression" } });
  expect(signal?.aborted).toBe(true);
  resolve(kind === "success" ? response() : new Response(JSON.stringify({code: kind === "invalid" ? "invalid_query" : "internal", detail: "Old failure", position: {offset:0,end:4}}), {status: kind === "invalid" ? 422 : 500}));
  await new Promise((done) => setTimeout(done, 30));
  await tick();
  expect(screen.queryByText(/Query validated|Old failure/)).toBeNull();
  expect((screen.getByLabelText("Query expression") as HTMLTextAreaElement).value).toBe("new expression");
});

it("selects a server byte span correctly without stealing focus during validation", async () => {
  vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ code:"invalid_query", detail:"Unknown operand", position:{offset:4,end:5} }), {status:422}));
  const { rerender } = open();
  await rerender({ query: { ...query, text: "😀x" } });
  await screen.findByText("Unknown operand");
  const expression = screen.getByLabelText("Query expression") as HTMLTextAreaElement;
  await fireEvent.click(screen.getByRole("button", {name:"Focus query error"}));
  expect(document.activeElement).toBe(expression);
  expect([expression.selectionStart, expression.selectionEnd]).toEqual([2,3]);
});

it("aborts pending validation on close and unmount", async () => {
  let resolve!: (value: Response) => void;
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(() => new Promise<Response>((done) => { resolve = done; }));
  const { onclose, unmount } = open();
  await waitFor(() => expect(fetch).toHaveBeenCalledTimes(1));
  await fireEvent.click(screen.getByRole("button", {name:"Close query editor"}));
  expect(fetch.mock.calls[0][1]?.signal?.aborted).toBe(true);
  expect(onclose).toHaveBeenCalledOnce();
  unmount();
  resolve(response());
  await tick();
  expect(screen.queryByRole("region", {name:"Query editor"})).toBeNull();
});

it("keeps a newer success when an older failure arrives after it", async () => {
  let old!: (value:Response)=>void;
  const fetch=vi.spyOn(globalThis,"fetch").mockImplementationOnce(()=>new Promise<Response>((resolve)=>{old=resolve;}))
    .mockImplementation(async (_input,init)=>response(String(init?.body)));
  open();
  await waitFor(()=>expect(fetch).toHaveBeenCalledTimes(1));
  await fireEvent.input(screen.getByLabelText("Query expression"),{target:{value:"new expression"}});
  await screen.findByText(/Query validated/);
  old(new Response(JSON.stringify({code:"internal",detail:"Obsolete failure"}),{status:500}));
  await new Promise((resolve)=>setTimeout(resolve,30));
  expect(screen.getByText(/Query validated/)).toBeTruthy();
  expect(screen.queryByText("Obsolete failure")).toBeNull();
});

it("invalidates the old session before accepting a new preview", async () => {
  let old!: (value:Response)=>void;
  const fetch=vi.spyOn(globalThis,"fetch").mockImplementationOnce(()=>new Promise<Response>((resolve)=>{old=resolve;}))
    .mockImplementation(async (_input,init)=>response(String(init?.body)));
  const {rerender}=open();
  await waitFor(()=>expect(fetch).toHaveBeenCalledTimes(1));
  await rerender({session:"replacement"});
  expect(fetch.mock.calls[0][1]?.signal?.aborted).toBe(true);
  old(new Response(JSON.stringify({code:"invalid_query",detail:"Old session"}),{status:422}));
  await screen.findByText(/Query validated/);
  expect(screen.queryByText("Old session")).toBeNull();
  expect(new Headers(fetch.mock.calls[1][1]?.headers).get("X-Docbank-Web-Session")).toBe("replacement");
});
