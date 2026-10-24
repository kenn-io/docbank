import { afterEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PeopleView from "./People.svelte";
import { People } from "./people.svelte.js";
import { HiddenAccess } from "./hiddenAccess.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { ada, grace, personMergeReceipt, personResponse } from "./people-test-fixtures.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
const unlockedAccess = () => {
  const access = new HiddenAccess("session", vi.fn());
  access.acceptState({ configured: true, expires_in: 60, expires_at: "later" }, Date.now());
  return access;
};
it.each([
  ["rename", true],
  ["rename", false],
  ["merge", true],
  ["merge", false],
] as const)(
  "confirms a lost %s response through browser reads, committed=%s",
  async (operation, committed) => {
    let rows = [ada, grace];
    const saved = {
      ...grace,
      display_name: operation === "rename" ? "Grace Hopper" : "Grace",
      revision: 5,
    };
    vi.stubGlobal("fetch", async (input: string, init: RequestInit) => {
      const url = new URL(input, "https://example.com");
      expect(new Headers(init.headers).get("X-Docbank-Web-Session")).toBe("session");
      if (url.pathname.startsWith("/api/v1/people/by-id/") && init.method === "GET")
        return Response.json({ detail: "Forbidden" }, { status: 403 });
      if (url.pathname === "/api/v1/people" && init.method === "GET") {
        expect(url.searchParams.get("limit")).toBe("24");
        expect(url.searchParams.get("photo_summary")).toBe("true");
        const query = url.searchParams.get("query") ?? "";
        if (operation === "merge" && query === "Ada") {
          // A same-name person on the first page cannot prove Ada was absorbed.
          if (!url.searchParams.has("cursor"))
            return Response.json({
              items: [{ ...ada, person_id: "other-ada" }],
              next_cursor: "next",
            });
          expect(url.searchParams.get("cursor")).toBe("next");
        }
        return Response.json({ items: rows.filter((row) => row.display_name.includes(query)) });
      }
      if (url.pathname === "/api/v1/people/by-id/grace" && init.method === "PATCH") {
        expect(JSON.parse(init.body as string)).toEqual({ display_name: "Grace Hopper" });
        if (committed) rows = [ada, saved];
        throw new TypeError("Response lost");
      }
      if (url.pathname === "/api/v1/people/by-id/grace/merge" && init.method === "POST") {
        expect(JSON.parse(init.body as string)).toMatchObject({ absorbed_person_id: "ada" });
        if (committed) rows = [saved];
        throw new TypeError("Response lost");
      }
      throw new Error(`Unexpected request: ${init.method} ${input}`);
    });
    const changed = vi.fn();
    const people = new People("session", vi.fn(), undefined, changed);
    people.items = [ada, grace];
    // The visible search can exclude the person we need to confirm.
    people.query = "Ada";
    const cache = new PhotoPreviewCache("session", vi.fn());
    render(PeopleView, { people, cache, onperson: vi.fn() });
    await act(async () => {
      if (operation === "rename") await people.rename(grace, "Grace Hopper");
      else {
        people.toggle(ada);
        people.toggle(grace);
        people.startMerge();
        people.keep(grace.person_id);
        people.confirmMerge(true);
        await people.submitMerge();
      }
    });
    if (committed)
      expect(changed).toHaveBeenCalledExactlyOnceWith(
        operation === "rename" ? "grace" : "ada",
        operation === "rename" ? "Grace Hopper" : "Grace",
        ...(operation === "merge" ? ["grace"] : []),
      );
    else expect(changed).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Ada 3 Library photos" })).toBeTruthy();
    expect(people.query).toBe("Ada");
    people.dispose();
    await cache.dispose();
  },
);
it.each([
  ["rename", 0, true],
  ["rename", 503, true],
  ["rename", 0, false],
  ["rename", 503, false],
  ["merge", 0, true],
  ["merge", 503, true],
  ["merge", 0, false],
  ["merge", 503, false],
] as const)(
  "requires reselection after uncertain %s HTTP %s, committed=%s",
  async (operation, status, committed) => {
    let rows = [ada, grace];
    const saved = { ...grace, display_name: "Grace Hopper", revision: 5 };
    const lost = () => {
      throw status ? new APIError("Response lost", status, "") : new TypeError("Response lost");
    };
    vi.spyOn(api, "listPeople").mockImplementation(async (params) => ({
      items: rows.filter((row) => row.display_name.includes(params?.query ?? "")),
    }));
    vi.spyOn(api, "renamePerson").mockImplementation(async () => {
      if (committed) rows = [ada, saved];
      return lost();
    });
    vi.spyOn(api, "mergePerson").mockImplementation(async (id, request, headers) => {
      if (operation === "merge") {
        if (committed) rows = [grace];
        return lost();
      }
      expect(id).toBe("grace");
      expect(request.absorbed_person_id).toBe("ada");
      expect(request.absorbed_revision).toBe(2);
      if (headers["If-Match"] !== '"5"')
        throw new APIError("Person changed", 412, "stale_revision");
      rows = [saved];
      return personMergeReceipt(ada, saved, request.operation_id);
    });
    const changed = vi.fn();
    const people = new People("session", vi.fn(), undefined, changed);
    people.items = rows;
    const cache = new PhotoPreviewCache("session", vi.fn());
    render(PeopleView, { people, cache, onperson: vi.fn() });
    await act(async () => {
      people.toggle(ada);
      people.toggle(grace);
      if (operation === "rename") await people.rename(grace, "Grace Hopper");
      else {
        people.startMerge();
        people.keep("grace");
        people.confirmMerge(true);
        await people.submitMerge();
      }
    });
    expect(people.selected).toEqual([]);
    for (const checkbox of screen.getAllByRole("checkbox"))
      expect((checkbox as HTMLInputElement).checked).toBe(false);
    expect(screen.queryByRole("button", { name: "Merge" })).toBeNull();
    if (operation === "rename" && committed) {
      await fireEvent.click(screen.getByRole("checkbox", { name: /^Select Grace Hopper/ }));
      await fireEvent.click(screen.getByRole("checkbox", { name: /^Select Ada/ }));
      await act(async () => {
        people.startMerge();
        people.keep("grace");
        people.confirmMerge(true);
        expect(await people.submitMerge()).toBe(true);
      });
      expect(changed).toHaveBeenLastCalledWith("ada", "Grace Hopper", "grace");
    }
    people.dispose();
    await cache.dispose();
  },
);
it("keeps the search text with the applied query when the view returns", async () => {
  const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada] });
  const people = new People("session", vi.fn());
  const cache = new PhotoPreviewCache("session", vi.fn());
  let view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.input(screen.getByRole("textbox", { name: "Find people" }), {
    target: { value: "Ad" },
  });
  await fireEvent.click(screen.getByRole("button", { name: "Search" }));
  await screen.findByRole("button", { name: "Ada 3 Library photos" });
  view.unmount();
  people.conceal();
  view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await people.load();
  expect(read.mock.calls.at(-1)![0]).toMatchObject({ query: "Ad" });
  expect((screen.getByRole("textbox", { name: "Find people" }) as HTMLInputElement).value).toBe(
    "Ad",
  );
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("submits a person rename from the dialog", async () => {
  vi.spyOn(api, "listPeople").mockResolvedValue({
    items: [{ ...grace, display_name: "Grace Hopper", revision: 5 }],
  });
  const access = new HiddenAccess("session", vi.fn());
  const people = new People("session", vi.fn(), access);
  people.items = [grace];
  const rename = vi
    .spyOn(api, "renamePerson")
    .mockImplementation(async (_id, _request, _headers) =>
      personResponse(grace, { display_name: "Grace Hopper", revision: 5 }),
    );
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Rename" }));
  const dialog = screen.getByRole("dialog", { name: "Rename Grace" });
  await fireEvent.input(within(dialog).getByRole("textbox", { name: "Person name" }), {
    target: { value: "Grace Hopper" },
  });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
  await waitFor(() => expect(rename).toHaveBeenCalledOnce());
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(rename).toHaveBeenCalledOnce();
  expect(rename.mock.calls[0]!.slice(0, 3)).toEqual([
    "grace",
    { display_name: "Grace Hopper" },
    { "If-Match": `"${grace.revision}"` },
  ]);
  expect(screen.getByRole("button", { name: "Grace Hopper 2 Library photos" })).toBeTruthy();
  view.unmount();
  people.dispose();
  access.dispose();
  await cache.dispose();
});
it.each([false, true])("submits selected cards and reports a locked merge=%s", async (locked) => {
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [grace] });
  const merge = vi
    .spyOn(api, "mergePerson")
    .mockImplementation(async (personID, request, _headers) => {
      if (locked) throw new APIError("unlock Hidden and retry the merge", 403, "hidden_locked");
      const source = [ada, grace].find(
        (person) => person.person_id === request.absorbed_person_id,
      )!;
      const survivor = [ada, grace].find((person) => person.person_id === personID)!;
      return personMergeReceipt(source, survivor, request.operation_id);
    });
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(
    screen.getByRole("checkbox", {
      name: `Select Ada · ${new Date(ada.created_at).toLocaleDateString()}`,
    }),
  );
  expect(screen.getByRole("button", { name: "Merge" }).hasAttribute("disabled")).toBe(true);
  await fireEvent.click(
    screen.getByRole("checkbox", {
      name: `Select Grace · ${new Date(grace.created_at).toLocaleDateString()}`,
    }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  const dialog = screen.getByRole("dialog", { name: "Merge people" });
  // Both cards show their Library evidence side by side.
  expect(within(dialog).getByText("3 Library photos")).toBeTruthy();
  expect(within(dialog).getByText("2 Library photos")).toBeTruthy();
  expect(within(dialog).queryByRole("checkbox", { name: "Confirm merge" })).toBeNull();
  const detail = new Date(grace.created_at).toLocaleDateString();
  await fireEvent.click(within(dialog).getByRole("radio", { name: `Keep Grace · ${detail}` }));
  const confirm = within(dialog).getByRole("button", { name: "Merge" });
  expect(confirm.hasAttribute("disabled")).toBe(true);
  await fireEvent.click(within(dialog).getByRole("checkbox", { name: "Confirm merge" }));
  await fireEvent.click(confirm);
  await waitFor(() => expect(merge).toHaveBeenCalledOnce());
  expect(merge.mock.calls[0]![0]).toBe("grace");
  expect(merge.mock.calls[0]![1].absorbed_person_id).toBe("ada");
  if (locked) expect(await screen.findByText("unlock Hidden and retry the merge")).toBeTruthy();
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("keeps portraits through in-place updates and releases them when cards clear", async () => {
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
  let nextURL = 0;
  const revokeObjectURL = vi.fn();
  vi.stubGlobal(
    "URL",
    Object.assign(URL, {
      createObjectURL: vi.fn(() => `blob:person-${++nextURL}`),
      revokeObjectURL,
    }),
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
          generation_id: "",
          rotation: 0,
          box: { x: 0.1, y: 0.1, width: 0.2, height: 0.2 },
        },
      },
    },
  ];
  vi.spyOn(api, "preparePhotoPreview").mockResolvedValue({
    state: "ready",
    generation_id: "ready",
  });
  const cache = new PhotoPreviewCache("session", vi.fn());
  const get = vi.spyOn(cache, "get").mockResolvedValue(new Blob(["synthetic"]));
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await waitFor(() => expect(screen.getByRole("img", { name: "Face of Ada" })).toBeTruthy());
  expect(get.mock.calls[0]![4]).toBe("ada");
  // An in-place card update with the same face keeps its portrait.
  await act(() => (people.items = [{ ...people.items[0]!, display_name: "Ada L", revision: 3 }]));
  expect(screen.getByRole("img", { name: "Face of Ada L" })).toBeTruthy();
  expect(get).toHaveBeenCalledOnce();
  await act(() => people.toggle(people.items[0]!));
  const current = people.items[0]!;
  vi.spyOn(api, "listPeople").mockResolvedValue({
    items: [
      {
        ...current,
        photo_summary: {
          ...current.photo_summary!,
          face: { ...current.photo_summary!.face!, generation_id: "ready" },
        },
      },
    ],
  });
  await act(() => people.load("Ada"));
  await waitFor(() => expect(screen.getByRole("img", { name: "Face of Ada L" })).toBeTruthy());
  await fireEvent.click(screen.getByRole("button", { name: "Clear" }));
  expect(screen.getByRole("img", { name: "Face of Ada L" })).toBeTruthy();
  const portraitURL = screen.getByRole("img", { name: "Face of Ada L" }).getAttribute("src");
  revokeObjectURL.mockClear();
  await act(() => people.conceal());
  expect(revokeObjectURL).toHaveBeenCalledWith(portraitURL);
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("labels namesakes by detail, and by ID only when the details match", async () => {
  const same = { ...grace, display_name: "Alex", display_name_folded: "alex" };
  const people = new People("session", vi.fn());
  people.items = [
    { ...same, person_id: "alex-1" },
    { ...same, person_id: "alex-2" },
    { ...same, person_id: "alex-3", identity_label: "alex@example.org" },
  ];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  const detail = new Date(same.created_at).toLocaleDateString();
  const checkboxes = [
    screen.getByRole("checkbox", { name: `Select Alex · ${detail} · alex-1` }),
    screen.getByRole("checkbox", { name: `Select Alex · ${detail} · alex-2` }),
    screen.getByRole("checkbox", { name: "Select Alex · alex@example.org" }),
  ];
  await fireEvent.click(checkboxes[0]!);
  await fireEvent.click(checkboxes[1]!);
  expect(
    within(screen.getByRole("region", { name: "Selected people" })).getByText(
      `Alex · ${detail} · alex-1 and Alex · ${detail} · alex-2 selected`,
    ),
  ).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  let dialog = screen.getByRole("dialog", { name: "Merge people" });
  await fireEvent.click(
    within(dialog).getByRole("radio", { name: `Keep Alex · ${detail} · alex-2` }),
  );
  expect(
    within(dialog).getByText(
      `Keep Alex · ${detail} · alex-2. Move Alex · ${detail} · alex-1's records into this person.`,
    ),
  ).toBeTruthy();
  await fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await fireEvent.click(checkboxes[1]!);
  await fireEvent.click(checkboxes[2]!);
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  dialog = screen.getByRole("dialog", { name: "Merge people" });
  expect(within(dialog).getByRole("radio", { name: `Keep Alex · ${detail}` })).toBeTruthy();
  expect(within(dialog).getByRole("radio", { name: "Keep Alex · alex@example.org" })).toBeTruthy();
  await fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
  await act(
    () => (people.items = [{ ...same, person_id: "alex-3", identity_label: "alex@example.org" }]),
  );
  expect(screen.getByRole("button", { name: "Alex 2 Library photos" })).toBeTruthy();
  view.unmount();
  people.dispose();
  await cache.dispose();
});

it("opens unlocked Hidden counts in Hidden and conceals them on lock", async () => {
  const access = unlockedAccess();
  const people = new People("session", vi.fn(), access);
  people.items = [ada, { ...grace, photo_summary: { library_count: 0, hidden_count: 1 } }];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const open = vi.fn();
  const view = render(PeopleView, { people, cache, onperson: open });
  await fireEvent.click(screen.getByRole("button", { name: "1 Hidden photo" }));
  expect(open).toHaveBeenLastCalledWith("grace", "Grace", true);
  await fireEvent.click(screen.getByRole("button", { name: "Grace 0 Library photos" }));
  expect(open).toHaveBeenLastCalledWith("grace", "Grace", true);
  // An open merge closes along with the Hidden counts.
  await fireEvent.click(
    screen.getByRole("checkbox", {
      name: `Select Ada · ${new Date(ada.created_at).toLocaleDateString()}`,
    }),
  );
  await fireEvent.click(
    screen.getByRole("checkbox", {
      name: `Select Grace · ${new Date(grace.created_at).toLocaleDateString()}`,
    }),
  );
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  const dialog = screen.getByRole("dialog", { name: "Merge people" });
  expect(within(dialog).getByText("3 Library photos")).toBeTruthy();
  await act(() => access.acceptState({ configured: true }, Date.now() + 1));
  expect(screen.queryByRole("dialog")).toBeNull();
  expect(screen.queryByRole("button", { name: "1 Hidden photo" })).toBeNull();
  view.unmount();
  people.dispose();
  access.dispose();
  await cache.dispose();
});
