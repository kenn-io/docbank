import * as actions from "./action-events.js";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import PhotoSearch from "./PhotoSearch.svelte";
import { Photos } from "./photos.svelte.js";
import { stubPhotoTimeline } from "./photo-test-fixtures.js";
import { tick } from "svelte";

vi.mock("./action-events.js", () => ({ reportAction: vi.fn() }));

beforeEach(() => {
  vi.mocked(actions.reportAction).mockClear();
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
  vi.restoreAllMocks();
  Reflect.deleteProperty(Element.prototype, "scrollIntoView");
});

it.each(["simple", "advanced"] as const)("submits %s text", async (syntax) => {
  const photos = new Photos("synthetic", vi.fn());
  photos.query = {
    ...photos.query,
    syntax,
    filters: { cameras: ["Synthetic camera"] },
  };
  const submit = vi.spyOn(photos, "setQuery").mockResolvedValue(undefined);
  render(PhotoSearch, { photos });
  const input = screen.getByRole("searchbox", { name: "Search photos" }) as HTMLInputElement;
  await fireEvent.input(input, { target: { value: "draft" } });
  await fireEvent.click(screen.getByRole("button", { name: "Clear search" }));
  expect(input.value).toBe("");
  expect(submit).not.toHaveBeenCalled();
  await fireEvent.input(input, { target: { value: "camera" } });
  await fireEvent.submit(input.closest("form")!);
  expect(submit).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({
      text: "camera",
      syntax,
      filters: { cameras: ["Synthetic camera"] },
    }),
  );
  photos.query = { ...photos.query, text: "camera" };
  await fireEvent.click(screen.getByRole("button", { name: "Clear search" }));
  expect(submit).toHaveBeenLastCalledWith(expect.objectContaining({ text: "", syntax }));
  photos.dispose();
});

it("restores applied text and mode when staged metadata prevents changing the search", async () => {
  const photos = new Photos("synthetic", vi.fn());
  photos.query = { ...photos.query, text: "camera" };
  photos.authored.stage({ caption: "Unapplied" });
  render(PhotoSearch, { photos });
  const input = screen.getByRole("searchbox", { name: "Search photos" }) as HTMLInputElement;
  await fireEvent.click(screen.getByRole("button", { name: "Clear search" }));
  expect(input.value).toBe("camera");
  expect(photos.query.text).toBe("camera");
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Plain words" }));
  await fireEvent.click(await screen.findByRole("option", { name: "Query syntax" }));
  expect(photos.query.syntax).toBe("simple");
  expect(screen.getByRole("combobox", { name: "Search mode: Plain words" })).toBeTruthy();
  photos.dispose();
});

it("preserves draft text and mode across facet changes and syncs applied values", async () => {
  const photos = new Photos("synthetic", vi.fn());
  render(PhotoSearch, { photos });
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Plain words" }));
  await fireEvent.click(await screen.findByRole("option", { name: "Query syntax" }));
  const input = screen.getByRole("searchbox", { name: "Search photos" }) as HTMLInputElement;
  await fireEvent.input(input, { target: { value: "rating:5" } });
  photos.query = { ...photos.query, filters: { cameras: ["Synthetic camera"] } };
  await tick();
  expect(input.value).toBe("rating:5");
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: "Search mode: Query syntax" })).toBeTruthy(),
  );
  photos.query = { ...photos.query, filters: {} };
  await tick();
  expect(input.value).toBe("rating:5");
  photos.query = { ...photos.query, text: "changed" };
  await waitFor(() => expect(input.value).toBe("changed"));
  photos.query = { ...photos.query, syntax: "advanced" };
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: "Search mode: Query syntax" })).toBeTruthy(),
  );
  photos.query = { ...photos.query, syntax: "simple" };
  await waitFor(() =>
    expect(screen.getByRole("combobox", { name: "Search mode: Plain words" })).toBeTruthy(),
  );
  photos.dispose();
});

it("disables search and its mode while a photo is hiding", async () => {
  const photos = new Photos("synthetic", vi.fn());
  render(PhotoSearch, { photos });
  const input = screen.getByRole("searchbox", { name: "Search photos" }) as HTMLInputElement;
  photos.hiding = true;
  await waitFor(() => expect(input.disabled).toBe(true));
  expect(
    screen.getByRole("combobox", { name: "Search mode: Plain words" }).hasAttribute("disabled"),
  ).toBe(true);
  photos.dispose();
});

it("applies the draft when the search mode changes", async () => {
  const photos = new Photos("synthetic", vi.fn());
  photos.query = { ...photos.query, syntax: "advanced" };
  const submit = vi.spyOn(photos, "setQuery").mockResolvedValue(undefined);
  render(PhotoSearch, { photos });
  await fireEvent.input(screen.getByRole("searchbox", { name: "Search photos" }), {
    target: { value: "rating:5" },
  });
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Query syntax" }));
  await fireEvent.click(await screen.findByRole("option", { name: "Plain words" }));
  expect(submit).toHaveBeenLastCalledWith(
    expect.objectContaining({ text: "rating:5", syntax: "simple" }),
  );
  photos.query = { ...photos.query, text: "camera" };
  await tick();
  submit.mockClear();
  await fireEvent.input(screen.getByRole("searchbox", { name: "Search photos" }), {
    target: { value: "" },
  });
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Plain words" }));
  await fireEvent.click(await screen.findByRole("option", { name: "Query syntax" }));
  expect(submit).toHaveBeenCalledExactlyOnceWith(
    expect.objectContaining({ text: "", syntax: "advanced" }),
  );
  photos.query = { ...photos.query, text: "" };
  await tick();
  submit.mockClear();
  await fireEvent.click(screen.getByRole("combobox", { name: "Search mode: Query syntax" }));
  await fireEvent.click(await screen.findByRole("option", { name: "Plain words" }));
  expect(submit).not.toHaveBeenCalled();
  photos.dispose();
});

it("reports completed photo searches while blank and canceled searches stay silent", async () => {
  stubPhotoTimeline();
  const action = vi.mocked(actions.reportAction);
  let finish!: (response: Response) => void;
  const fetcher = vi.fn(() => Promise.resolve(Response.json({ items: [], total: 0, facets: [] })));
  vi.stubGlobal("fetch", fetcher);
  const photos = new Photos("scoped", vi.fn());
  photos.started = true;
  const submit = vi.spyOn(photos, "setQuery");
  render(PhotoSearch, { photos });
  const search = screen.getByRole("searchbox", { name: "Search photos" });
  await fireEvent.input(search, { target: { value: "   " } });
  await fireEvent.submit(search.closest("form")!);
  await waitFor(() => expect(photos.loading).toBe(false));
  expect(action).not.toHaveBeenCalled();
  await fireEvent.input(search, { target: { value: "Nikon" } });
  await fireEvent.submit(search.closest("form")!);
  await waitFor(() =>
    expect(action).toHaveBeenCalledExactlyOnceWith("scoped", "search_run", "text"),
  );
  action.mockClear();
  fetcher.mockClear();
  submit.mockClear();
  fetcher.mockImplementation(() => new Promise<Response>((resolve) => (finish = resolve)));
  await fireEvent.input(search, { target: { value: "river" } });
  await fireEvent.submit(search.closest("form")!);
  expect(fetcher).toHaveBeenCalledOnce();
  photos.dispose();
  await act(async () => {
    finish(Response.json({ items: [], total: 0, facets: [] }));
    await submit.mock.results[0].value;
  });
  expect(action).not.toHaveBeenCalled();
});
