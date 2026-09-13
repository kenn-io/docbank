import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/svelte";
import App from "./App.svelte";

afterEach(() => { cleanup(); history.replaceState(null, "", "/"); Reflect.deleteProperty(Element.prototype, "scrollIntoView"); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

it("restores a full query beside sign-in without running it as empty-text folder browsing", async () => {
  const query = { v: 1, text: "", syntax: "advanced", mode: "hybrid", filters: { extensions: ["pdf"], no_tags: true }, sort: { field: "size", direction: "desc" } };
  history.replaceState(null, "", `/#web_session=secret&web_upload_secret=proof&query=${encodeURIComponent(JSON.stringify(query))}`);
  vi.stubGlobal("ResizeObserver", class { observe() {} unobserve() {} disconnect() {} });
  Object.defineProperty(Element.prototype, "scrollIntoView", { configurable: true, value: vi.fn() });
  const root = { id: 1, name: "", kind: "dir", path: "/", revision: 1, size: 0, created_at: "2026-01-01T00:00:00Z", modified_at: "2026-01-01T00:00:00Z" };
  const tag = { id: "11111111-1111-4111-8111-111111111111", name: "review", revision: 1, assignment_count: 0 };
  const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = String(input);
    const value = url.includes("/path?") ? root : url.includes("/children?") ? { directory: root, items: [], total: 0, limit: 1000, offset: 0 }
      : url === "/api/v1/tags?limit=1000&offset=0" ? { items: [tag], total: 1, limit: 1000, offset: 0 }
      : url.includes("/nodes?") ? { items: [], total: 0, limit: 1000, offset: 0, omitted_trashed: 0 }
      : { items: [], total: 0, limit: url.includes("saved-queries") ? 100 : 1000, offset: 0 };
    return new Response(JSON.stringify(value));
  });
  render(App);
  await screen.findByRole("region", { name: "Query editor" });
  expect(JSON.parse(new URLSearchParams(location.hash.slice(1)).get("query")!)).toEqual(query);
  expect(location.hash).not.toContain("web_session");
  expect(location.hash).not.toContain("proof");
  expect(fetch.mock.calls.some(([url]) => String(url).includes("/search"))).toBe(false);
  await fireEvent.click(screen.getByRole("button", { name: "Close query editor" }));
  await fireEvent.click(screen.getByRole("button", { name: "Saved queries and highlights" }));
  expect(JSON.parse((screen.getByLabelText("Complete query JSON") as HTMLTextAreaElement).value)).toEqual(query);
  const kept = { ...query, text: "new draft" };
  await fireEvent.input(screen.getByLabelText("Complete query JSON"), { target: { value: JSON.stringify(kept) } });
  await fireEvent.click(screen.getByRole("button", { name: "Keep query draft" }));
  await fireEvent.click(screen.getByRole("button", { name: "New query" }));
  expect(JSON.parse((screen.getByLabelText("Complete query JSON") as HTMLTextAreaElement).value)).toEqual(kept);
  await fireEvent.click(screen.getByRole("button", { name: "Close" }));
  await fireEvent.click(screen.getByRole("button", { name: "Discard query draft" }));
  await fireEvent.input(screen.getByRole("searchbox", { name: "Search documents" }), { target: { value: "x".repeat(8193) } });
  await fireEvent.click(screen.getByRole("button", { name: "Saved queries and highlights" }));
  await screen.findByRole("dialog", { name: "Saved queries and highlights" });
  expect(JSON.parse((screen.getByLabelText("Complete query JSON") as HTMLTextAreaElement).value).text).toBe("x".repeat(8193));
  await fireEvent.click(screen.getByRole("button", { name: "Keep query draft" }));
  expect((await screen.findByRole("alert")).textContent).toContain("8192");
  await fireEvent.click(screen.getByRole("button", { name: "New highlight set" }));
  expect(screen.getByLabelText("Term 1")).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Close" }));
  await fireEvent.input(screen.getByRole("searchbox", { name: "Search documents" }), { target: { value: "" } });
  await fireEvent.click(screen.getByRole("combobox", { name: "Browse or filter by tag: All tags" }));
  await fireEvent.click(screen.getByRole("option", { name: "review (0)" }));
  await screen.findByText("Documents tagged");
  await fireEvent.click(screen.getByRole("button", { name: "Saved queries and highlights" }));
  expect(JSON.parse((screen.getByLabelText("Complete query JSON") as HTMLTextAreaElement).value).sort).toEqual({ field: "path", direction: "asc" });
});

it("shows malformed query fragments on the sign-in screen", async () => {
  history.replaceState(null, "", "/#query=%7Bbad");
  render(App);
  expect((await screen.findByRole("alert")).textContent).toContain("Query URL");
});
