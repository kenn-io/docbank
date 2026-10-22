import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PeopleView from "./People.svelte";
import { People } from "./people.svelte.js";
import { HiddenAccess } from "./hiddenAccess.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import * as api from "./generated/docbank.js";

const ada: api.PersonSummary = {
  person_id: "ada",
  display_name: "Ada",
  display_name_folded: "ada",
  revision: 2,
  state: "curated",
  created_at: "2026-01-01",
  photo_summary: { library_count: 3, hidden_count: 1 },
};
const grace: api.PersonSummary = {
  ...ada,
  person_id: "grace",
  display_name: "Grace",
  display_name_folded: "grace",
  revision: 4,
  photo_summary: { library_count: 0 },
};
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
beforeEach(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
      unobserve() {}
    },
  );
});
it("opens Library and Hidden counts, including cards without photo evidence", async () => {
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const open = vi.fn();
  const view = render(PeopleView, { people, cache, onperson: open });
  await fireEvent.click(screen.getByRole("link", { name: "Ada 3 Library photos" }));
  expect(open).toHaveBeenCalledWith("ada", "Ada");
  await fireEvent.click(screen.getByRole("button", { name: "1 Hidden photo" }));
  expect(open).toHaveBeenCalledWith("ada", "Ada", true);
  expect(screen.getByRole("link", { name: "Grace 0 Library photos" })).toBeTruthy();
  await fireEvent.click(screen.getAllByRole("button", { name: "Rename" })[1]!);
  expect(screen.getByRole("dialog", { name: "Rename Grace" })).toBeTruthy();
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("renames a zero-photo person using the current revision", async () => {
  const people = new People("session", vi.fn());
  people.items = [grace];
  vi.spyOn(api, "listPeople").mockResolvedValue({
    items: [{ ...grace, display_name: "Grace Hopper" }],
  });
  const rename = vi.spyOn(api, "renamePerson").mockResolvedValue({} as api.Person);
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Rename" }));
  const dialog = screen.getByRole("dialog", { name: "Rename Grace" });
  await fireEvent.input(within(dialog).getByRole("textbox", { name: "Person name" }), {
    target: { value: "Grace Hopper" },
  });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(rename).toHaveBeenCalledWith(
    "grace",
    { display_name: "Grace Hopper" },
    { "If-Match": '"4"' },
    expect.anything(),
  );
  expect(screen.getByRole("link", { name: "Grace Hopper 0 Library photos" })).toBeTruthy();
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("chooses the merge survivor by ID and requires confirmation", async () => {
  const people = new People("session", vi.fn());
  people.items = [ada];
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada, grace] });
  const merge = vi.spyOn(api, "mergePerson").mockResolvedValue({} as api.PersonMergeReceipt);
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  const dialog = screen.getByRole("dialog", { name: "Merge Ada" });
  await fireEvent.click(within(dialog).getByRole("button", { name: /Find the person to keep/ }));
  await waitFor(() => expect(screen.getByRole("option", { name: "Grace · grace" })).toBeTruthy());
  await fireEvent.mouseDown(screen.getByRole("option", { name: "Grace · grace" }));
  const confirm = within(dialog).getByRole("button", { name: "Merge" });
  expect(confirm.hasAttribute("disabled")).toBe(true);
  await fireEvent.click(within(dialog).getByRole("checkbox", { name: "Confirm merge" }));
  await fireEvent.click(confirm);
  await waitFor(() => expect(merge).toHaveBeenCalled());
  expect(merge.mock.calls[0]![0]).toBe("grace");
  expect(merge.mock.calls[0]![1].absorbed_person_id).toBe("ada");
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("clears an open merge search and its late response when Hidden locks", async () => {
  const access = new HiddenAccess("session", vi.fn());
  access.acceptState({ configured: true, expires_in: 60, expires_at: "later" }, Date.now());
  const people = new People("session", vi.fn(), access);
  people.items = [ada];
  let finish!: (page: api.PersonPage) => void;
  const read = vi.spyOn(api, "listPeople").mockImplementation(
    () =>
      new Promise((resolve) => {
        finish = resolve;
      }),
  );
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  await waitFor(() => expect(read).toHaveBeenCalled());
  await act(() => access.acceptState({ configured: true }, Date.now() + 1));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(read.mock.calls[0]![1]!.signal!.aborted).toBe(true);
  finish({ items: [grace] });
  await act(() => Promise.resolve());
  expect(screen.queryByText("Grace")).toBeNull();
  view.unmount();
  people.dispose();
  access.dispose();
  await cache.dispose();
});
it("releases person-scoped preview URLs when cards clear", async () => {
  vi.stubGlobal(
    "Image",
    class {
      src = "";
      naturalWidth = 1000;
      naturalHeight = 800;
      decode() {
        return Promise.resolve();
      }
    },
  );
  vi.stubGlobal(
    "URL",
    Object.assign(URL, { createObjectURL: vi.fn(() => "blob:person"), revokeObjectURL: vi.fn() }),
  );
  const people = new People("session", vi.fn());
  people.items = [
    {
      ...ada,
      photo_summary: {
        library_count: 1,
        face: {
          asset_id: "asset",
          content_version_id: "version",
          generation_id: "ready",
          rotation: 0,
          box: { x: 0.1, y: 0.1, width: 0.2, height: 0.2 },
        },
      },
    },
  ];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const get = vi.spyOn(cache, "get").mockResolvedValue(new Blob(["synthetic"]));
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await waitFor(() => expect(screen.getByRole("img", { name: "Face of Ada" })).toBeTruthy());
  expect(get.mock.calls[0]![4]).toBe("ada");
  await act(() => people.conceal());
  expect(URL.revokeObjectURL).toHaveBeenCalledWith("blob:person");
  view.unmount();
  people.dispose();
  await cache.dispose();
});
