import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/svelte";
import PeopleView from "./People.svelte";
import { People } from "./people.svelte.js";
import { HiddenAccess } from "./hiddenAccess.svelte.js";
import { PhotoPreviewCache } from "./photoPreviewCache.js";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import {
  ada,
  candidate,
  candidateFace,
  f1,
  f3,
  isAda,
  unnamed,
  decided,
  grace,
  personMergeReceipt,
  personResponse,
} from "./people-test-fixtures.js";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
// Cards load face suggestions too; most cases have none.
beforeEach(() => {
  vi.spyOn(api, "listFaceCandidates").mockResolvedValue({ items: [], total: 0 });
});
const unlockedAccess = () => {
  const access = new HiddenAccess("session", vi.fn());
  access.acceptState({ configured: true, expires_in: 60, expires_at: "later" }, Date.now());
  return access;
};
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
it.each([false, true])("submits the dialog's rename revision after refresh=%s", async (refresh) => {
  const current = { ...grace, display_name: "Grace Murray", revision: 5 };
  vi.spyOn(api, "listPeople").mockResolvedValue({
    items: [refresh ? current : { ...grace, display_name: "Grace Hopper", revision: 5 }],
  });
  const access = new HiddenAccess("session", vi.fn());
  const people = new People("session", vi.fn(), access);
  people.items = [grace];
  const rename = vi
    .spyOn(api, "renamePerson")
    .mockImplementation(async (_id, _request, headers) => {
      if (refresh && headers?.["If-Match"] !== '"5"')
        throw new APIError("Person changed; review the current name", 412, "stale_revision");
      return personResponse(grace, { display_name: "Grace Hopper", revision: 5 });
    });
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await fireEvent.click(screen.getByRole("button", { name: "Rename" }));
  const dialog = screen.getByRole("dialog", { name: "Rename Grace" });
  if (refresh) await act(() => people.refresh());
  await fireEvent.input(within(dialog).getByRole("textbox", { name: "Person name" }), {
    target: { value: "Grace Hopper" },
  });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Rename" }));
  await waitFor(() => expect(rename).toHaveBeenCalledOnce());
  if (refresh) {
    expect((await within(dialog).findByRole("alert")).textContent).toBe(
      "Person changed; review the current name",
    );
    expect(
      await screen.findByRole("button", { name: "Grace Murray 2 Library photos" }),
    ).toBeTruthy();
  } else {
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByRole("button", { name: "Grace Hopper 2 Library photos" })).toBeTruthy();
  }
  expect(rename).toHaveBeenCalledOnce();
  expect(rename.mock.calls[0]!.slice(0, 3)).toEqual([
    grace.person_id,
    { display_name: "Grace Hopper" },
    { "If-Match": `"${grace.revision}"` },
  ]);
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
  expect(merge.mock.calls[0]![0]).toBe(grace.person_id);
  expect(merge.mock.calls[0]![1].absorbed_person_id).toBe(ada.person_id);
  if (locked) expect(await screen.findByText("unlock Hidden and retry the merge")).toBeTruthy();
  else await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
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
    class extends URL {
      static createObjectURL = vi.fn(() => `blob:person-${++nextURL}`);
      static revokeObjectURL = revokeObjectURL;
    },
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
  expect(get.mock.calls[0]![4]).toBe(ada.person_id);
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
  await waitFor(() => expect(get).toHaveBeenCalledTimes(2));
  const portraitURL = screen.getByRole("img", { name: "Face of Ada L" }).getAttribute("src");
  revokeObjectURL.mockClear();
  await act(() => people.conceal());
  expect(revokeObjectURL).toHaveBeenCalledWith(portraitURL);
  view.unmount();
  people.dispose();
  await cache.dispose();
});
it("labels namesakes by detail, and by ID only when the details match", async () => {
  const alex1 = "00000000-0000-4000-8000-000000000011";
  const alex2 = "00000000-0000-4000-8000-000000000012";
  const alex3 = "00000000-0000-4000-8000-000000000013";
  const same = { ...grace, display_name: "Alex", display_name_folded: "alex" };
  const people = new People("session", vi.fn());
  people.items = [
    { ...same, person_id: alex1 },
    { ...same, person_id: alex2 },
    { ...same, person_id: alex3, identity_label: "alex@example.org" },
  ];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  const detail = new Date(same.created_at).toLocaleDateString();
  const checkboxes = [
    screen.getByRole("checkbox", { name: `Select Alex · ${detail} · ${alex1}` }),
    screen.getByRole("checkbox", { name: `Select Alex · ${detail} · ${alex2}` }),
    screen.getByRole("checkbox", { name: "Select Alex · alex@example.org" }),
  ];
  await fireEvent.click(checkboxes[0]!);
  await fireEvent.click(checkboxes[1]!);
  expect(
    within(screen.getByRole("region", { name: "Selected people" })).getByText(
      `Alex · ${detail} · ${alex1} and Alex · ${detail} · ${alex2} selected`,
    ),
  ).toBeTruthy();
  await fireEvent.click(screen.getByRole("button", { name: "Merge" }));
  let dialog = screen.getByRole("dialog", { name: "Merge people" });
  await fireEvent.click(
    within(dialog).getByRole("radio", { name: `Keep Alex · ${detail} · ${alex2}` }),
  );
  expect(
    within(dialog).getByText(
      `Keep Alex · ${detail} · ${alex2}. Move Alex · ${detail} · ${alex1}'s records into this person.`,
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
    () => (people.items = [{ ...same, person_id: alex3, identity_label: "alex@example.org" }]),
  );
  expect(screen.getByRole("button", { name: "Alex 2 Library photos" })).toBeTruthy();
  view.unmount();
  people.dispose();
  await cache.dispose();
});

it("opens unlocked Hidden counts in Hidden and conceals them on lock", async () => {
  const access = unlockedAccess();
  const people = new People("session", vi.fn(), access);
  people.items = [
    { ...ada, photo_summary: { library_count: 3, hidden_count: 2 } },
    { ...grace, photo_summary: { library_count: 0, hidden_count: 1 } },
    {
      ...grace,
      person_id: "00000000-0000-4000-8000-000000000003",
      display_name: "Lin",
      photo_summary: { library_count: 1, hidden_count: 0 },
    },
  ];
  const cache = new PhotoPreviewCache("session", vi.fn());
  const open = vi.fn();
  const view = render(PeopleView, { people, cache, onperson: open });
  expect(screen.queryByRole("button", { name: "0 Hidden photos" })).toBeNull();
  await fireEvent.click(screen.getByRole("button", { name: "2 Hidden photos" }));
  expect(open).toHaveBeenLastCalledWith(ada.person_id, "Ada", true);
  await fireEvent.click(screen.getByRole("button", { name: "1 Hidden photo" }));
  expect(open).toHaveBeenLastCalledWith(grace.person_id, "Grace", true);
  await fireEvent.click(screen.getByRole("button", { name: "Grace 0 Library photos" }));
  expect(open).toHaveBeenLastCalledWith(grace.person_id, "Grace", true);
  // An access-scope change closes the merge dialog.
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
  // App conceals the old cards before loading the locked projection.
  await act(() => people.conceal());
  expect(screen.queryByRole("button", { name: "1 Hidden photo" })).toBeNull();
  expect(screen.queryByRole("button", { name: "2 Hidden photos" })).toBeNull();
  view.unmount();
  people.dispose();
  access.dispose();
  await cache.dispose();
});

const stubImages = () => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  );
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
  let next = 0;
  vi.stubGlobal(
    "URL",
    class extends URL {
      static createObjectURL = vi.fn(() => `blob:face-${++next}`);
      static revokeObjectURL = vi.fn();
    },
  );
};
async function renderSuggestions(rows: ReturnType<typeof candidate>[], access?: HiddenAccess) {
  stubImages();
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada] });
  const list = vi
    .spyOn(api, "listFaceCandidates")
    .mockResolvedValue({ items: rows, total: rows.length });
  const people = new People("session", vi.fn(), access);
  const cache = new PhotoPreviewCache("session", vi.fn());
  const get = vi.spyOn(cache, "get").mockResolvedValue(new Blob(["synthetic"]));
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await act(() => people.load());
  const region = await screen.findByRole("region", { name: /Suggestions/ });
  const done = async () => {
    view.unmount();
    people.dispose();
    await cache.dispose();
  };
  return { people, list, get, region, done };
}

it("shows suggestion cards with unnamed crops and no row when there are none", async () => {
  const { region, get, list, people, done } = await renderSuggestions([
    isAda,
    { ...unnamed, faces: [candidateFace("face-4"), candidateFace("face-5")] },
  ]);
  const cards = within(region).getAllByRole("button");
  expect(cards.map((card) => card.textContent?.replace(/\s+/g, " ").trim())).toEqual([
    "Is this Ada? 3 faces · 2 photos",
    "Who is this? 2 faces · 1 photo",
  ]);
  // Each card requests its own exact-source face crop.
  await waitFor(() => expect(within(region).getAllByRole("img")).toHaveLength(2));
  expect(get.mock.calls.map((call) => [call[0], call[1], call[4], call[5]])).toEqual([
    ["asset-face-1", "generation-face-1", undefined, "face-1"],
    ["asset-face-4", "generation-face-4", undefined, "face-4"],
  ]);
  list.mockResolvedValue({ items: [], total: 0 });
  await act(() => people.suggestions.load());
  expect(screen.queryByRole("region", { name: /Suggestions/ })).toBeNull();
  await done();
});

it("renders retry and show-more controls for suggestions", async () => {
  stubImages();
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada] });
  const rows = Array.from({ length: 30 }, (_, index) =>
    candidate(`candidate-${index}`, [candidateFace(`face-${index}`)]),
  );
  vi.spyOn(api, "listFaceCandidates")
    .mockRejectedValueOnce(new APIError("Suggestions unavailable", 503, "unavailable"))
    .mockImplementation(async (params) => ({
      items: rows.slice(params!.offset!, params!.offset! + params!.limit!),
      total: rows.length,
    }));
  const people = new People("session", vi.fn());
  const cache = new PhotoPreviewCache("session", vi.fn());
  vi.spyOn(cache, "get").mockResolvedValue(new Blob(["synthetic"]));
  const view = render(PeopleView, { people, cache, onperson: vi.fn() });
  await act(() => people.load());
  const region = await screen.findByRole("region", { name: /Suggestions/ });
  expect((await within(region).findByRole("alert")).textContent).toContain(
    "Suggestions unavailable",
  );
  await fireEvent.click(within(region).getByRole("button", { name: "Retry" }));
  await within(region).findByRole("button", { name: "Show more" });
  await fireEvent.click(within(region).getByRole("button", { name: "Show more" }));
  await waitFor(() =>
    expect(within(region).queryByRole("button", { name: "Show more" })).toBeNull(),
  );
  view.unmount();
  people.dispose();
  await cache.dispose();
});

it("confirms an Is this X? group after removing a face", async () => {
  const { region, list, done } = await renderSuggestions([isAda]);
  vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "r" });
  const decide = vi.spyOn(api, "decideFaceCandidate").mockResolvedValue(decided(isAda));
  await fireEvent.click(within(region).getByRole("button", { name: /Is this Ada\?/ }));
  const dialog = screen.getByRole("dialog", { name: "Is this Ada?" });
  expect(within(dialog).getByText("3 faces · 2 photos")).toBeTruthy();
  expect(within(dialog).getByText("Remove anyone who doesn't belong, then confirm.")).toBeTruthy();
  await waitFor(() => expect(within(dialog).getAllByRole("img")).toHaveLength(3));
  const regrouped = { ...isAda, candidate_id: "candidate-ada-2", faces: [f1, f3], revision: 1 };
  list.mockResolvedValue({ items: [regrouped], total: 1 });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Not Ada: face 2" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await fireEvent.click(within(region).getByRole("button", { name: /Is this Ada\?/ }));
  const reopened = screen.getByRole("dialog", { name: "Is this Ada?" });
  list.mockResolvedValue({ items: [], total: 0 });
  const confirm = within(reopened).getByRole("button", { name: "Confirm" });
  await waitFor(() => expect(confirm.hasAttribute("disabled")).toBe(false));
  await fireEvent.click(confirm);
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(decide).toHaveBeenCalledOnce();
  await waitFor(() => expect(screen.queryByRole("region", { name: /Suggestions/ })).toBeNull());
  await done();
});

it("rejects an Is this X? group and reports a stale decision in the sheet", async () => {
  const { region, list, done } = await renderSuggestions([isAda]);
  vi.spyOn(api, "decideFaceCandidate")
    .mockRejectedValueOnce(new APIError("Suggestion changed", 412, "stale_revision"))
    .mockResolvedValue(decided(isAda));
  await fireEvent.click(within(region).getByRole("button", { name: /Is this Ada\?/ }));
  const dialog = screen.getByRole("dialog", { name: "Is this Ada?" });
  list.mockResolvedValue({ items: [{ ...isAda, revision: 7 }], total: 1 });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Not Ada" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect((await screen.findByRole("alert")).textContent).toBe("Suggestion changed");
  await done();
});

it("shows a spinner for each review crop until it loads or fails", async () => {
  const { region, get, done } = await renderSuggestions([unnamed]);
  const loads: { resolve: (blob: Blob) => void; reject: (cause: unknown) => void }[] = [];
  get.mockImplementation(
    () => new Promise<Blob>((resolve, reject) => loads.push({ resolve, reject })),
  );
  await fireEvent.click(within(region).getByRole("button", { name: /Who is this\?/ }));
  const dialog = screen.getByRole("dialog", { name: "Who is this?" });
  await waitFor(() => expect(loads).toHaveLength(2));
  expect(within(dialog).getAllByRole("status", { name: "Loading face" })).toHaveLength(2);
  expect(dialog.querySelector(".lucide-user")).toBeNull();
  await act(() => loads[0]!.resolve(new Blob(["synthetic"])));
  await waitFor(() => expect(within(dialog).getAllByRole("img")).toHaveLength(1));
  expect(within(dialog).getAllByRole("status", { name: "Loading face" })).toHaveLength(1);
  await act(() => loads[1]!.reject(new Error("Preview unavailable")));
  await waitFor(() =>
    expect(within(dialog).queryByRole("status", { name: "Loading face" })).toBeNull(),
  );
  expect(dialog.querySelectorAll(".lucide-user")).toHaveLength(1);
  await done();
});

it("skips faces until nothing is left in a Who is this? group", async () => {
  const { region, list, done } = await renderSuggestions([unnamed]);
  vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "r" });
  await fireEvent.click(within(region).getByRole("button", { name: /Who is this\?/ }));
  const dialog = screen.getByRole("dialog", { name: "Who is this?" });
  list.mockResolvedValue({ items: [], total: 0 });
  await fireEvent.click(within(dialog).getByRole("button", { name: "Skip face 1" }));
  await waitFor(() =>
    expect(within(dialog).getAllByRole("button", { name: /Skip face/ })).toHaveLength(1),
  );
  await fireEvent.click(within(dialog).getByRole("button", { name: "Skip face 1" }));
  expect(await within(dialog).findByText("Nothing left to review in this group.")).toBeTruthy();
  const finish = within(dialog).getByRole("button", { name: "Done" });
  await waitFor(() => expect(finish.hasAttribute("disabled")).toBe(false));
  await fireEvent.click(finish);
  expect(screen.queryByRole("dialog")).toBeNull();
  await done();
});

it.each(["existing", "new", "dismiss"])(
  "names or dismisses a Who is this? group: %s",
  async (choice) => {
    const { region, people, list, done } = await renderSuggestions([unnamed]);
    const decide = vi.spyOn(api, "decideFaceCandidate").mockResolvedValue(decided(unnamed));
    await fireEvent.click(within(region).getByRole("button", { name: /Who is this\?/ }));
    const dialog = screen.getByRole("dialog", { name: "Who is this?" });
    const confirm = within(dialog).getByRole("button", { name: "Confirm" });
    expect(confirm.hasAttribute("disabled")).toBe(true);
    if (choice === "dismiss") {
      await fireEvent.click(within(dialog).getByRole("button", { name: "Dismiss" }));
    } else {
      const search = vi.mocked(api.listPeople);
      await fireEvent.click(within(dialog).getByRole("button", { name: "Name this person" }));
      await fireEvent.input(screen.getByRole("combobox"), {
        target: { value: choice === "new" ? "Ana" : "Ad" },
      });
      if (choice === "new") search.mockResolvedValue({ items: [] });
      await fireEvent.mouseDown(
        await screen.findByRole("option", { name: choice === "new" ? 'New person "Ana"' : "Ada" }),
      );
      await waitFor(() => expect(confirm.hasAttribute("disabled")).toBe(false));
      list.mockResolvedValue({
        items: [{ ...unnamed, candidate_id: "candidate-rebuilt", revision: 5 }],
        total: 1,
      });
      await act(() => people.suggestions.refresh());
      expect(confirm.hasAttribute("disabled")).toBe(false);
      await fireEvent.click(confirm);
    }
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(decide).toHaveBeenCalledOnce();
    await done();
  },
);

it("chooses a person after paging and retrying the name picker", async () => {
  const { region, done } = await renderSuggestions([unnamed]);
  const alex = ["11", "12"].map((id) => ({
    ...grace,
    person_id: `00000000-0000-4000-8000-0000000000${id}`,
    display_name: "Alex",
    display_name_folded: "alex",
  }));
  const decide = vi.spyOn(api, "decideFaceCandidate").mockResolvedValue(decided(unnamed));
  let failed = false;
  vi.mocked(api.listPeople).mockImplementation(async (query) => {
    if (!query?.cursor)
      return { items: [ada, alex[0]!], next_cursor: "page-2", same_name_continues: true };
    if (!failed) throw ((failed = true), new APIError("People unavailable", 503, "unavailable"));
    return { items: [ada, alex[1]!] };
  });
  await fireEvent.click(within(region).getByRole("button", { name: /Who is this\?/ }));
  await fireEvent.click(screen.getByRole("button", { name: "Name this person" }));
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "A" } });
  // Exercise the People view's shared chooser and paging callbacks.
  const more = await screen.findByRole("option", { name: "Load more" });
  await fireEvent.mouseDown(more);
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Retry" }));
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Alex · 2 people" }));
  const chooser = within(await screen.findByRole("dialog", { name: "Choose Alex" }));
  await fireEvent.click(chooser.getByRole("radio", { name: new RegExp(alex[1]!.person_id) }));
  await fireEvent.click(chooser.getByRole("button", { name: "Choose" }));
  const review = within(await screen.findByRole("dialog", { name: "Who is this?" }));
  await fireEvent.click(review.getByRole("button", { name: "Confirm" }));
  await waitFor(() => expect(decide).toHaveBeenCalledOnce());
  expect(decide.mock.calls[0]![1]).toMatchObject({ person_id: alex[1]!.person_id });
  await done();
});

it("closes the review sheet when the Hidden scope changes", async () => {
  const access = unlockedAccess();
  const { region, done } = await renderSuggestions([isAda], access);
  await fireEvent.click(within(region).getByRole("button", { name: /Is this Ada\?/ }));
  expect(screen.getByRole("dialog", { name: "Is this Ada?" })).toBeTruthy();
  await act(() => access.scope++);
  expect(screen.queryByRole("dialog")).toBeNull();
  await done();
  access.dispose();
});
