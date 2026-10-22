import { afterEach, expect, it, vi } from "vite-plus/test";
import { People, peoplePageSize } from "./people.svelte.js";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { ada, grace, person } from "./people-test-fixtures.js";

afterEach(() => {
  vi.restoreAllMocks();
});
const byID = (rows: api.PersonSummary[]) => (params?: api.ListPeopleParams) =>
  Promise.resolve({ items: rows.filter((row) => row.person_id === params?.person_id) });
const merging = (people: People, source: api.PersonSummary, survivor: api.PersonSummary) => {
  people.startMerge(source);
  people.chooseSurvivor(survivor);
  people.confirmMerge(true);
};

it("pages the projection and cancels a late scoped response", async () => {
  let finish!: (value: api.PersonPage) => void;
  const read = vi
    .spyOn(api, "listPeople")
    .mockResolvedValueOnce({ items: [ada], next_cursor: "next" })
    .mockResolvedValueOnce({ items: [grace] })
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    );
  const people = new People("session", vi.fn());
  await people.load();
  await people.load("", true);
  expect(people.items.map((row) => row.person_id)).toEqual(["ada", "grace"]);
  expect(read.mock.calls[1]![0]).toEqual({
    query: "",
    limit: peoplePageSize,
    photo_summary: true,
    cursor: "next",
  });
  const pending = people.load("Grace");
  people.conceal();
  finish({ items: [grace] });
  await pending;
  expect(people.items).toEqual([]);
  expect(read.mock.calls[2]![1]!.signal!.aborted).toBe(true);
  people.dispose();
});

it("renames a card on page 3 in place without reloading the list", async () => {
  const pages = [1, 2, 3].map((page) => ({
    items: [person(`p${page}`)],
    next_cursor: page < 3 ? `c${page}` : undefined,
  }));
  const renamed = person("p3", { display_name: "Zoe", display_name_folded: "zoe", revision: 5 });
  const read = vi
    .spyOn(api, "listPeople")
    .mockResolvedValueOnce(pages[0]!)
    .mockResolvedValueOnce(pages[1]!)
    .mockResolvedValueOnce(pages[2]!)
    .mockResolvedValueOnce({ items: [renamed] });
  vi.spyOn(api, "renamePerson").mockResolvedValue({
    ...renamed,
    origin: "operator",
    updated_at: "2026-01-02",
  } as api.Person);
  const changed = vi.fn();
  const people = new People("session", vi.fn(), undefined, changed);
  await people.load();
  await people.load(people.query, true);
  await people.load(people.query, true);
  expect(await people.rename(people.items[2]!, " Zoe ")).toBe(true);
  expect(people.items.map((row) => row.display_name)).toEqual(["p1", "p2", "Zoe"]);
  expect(people.cursor).toBeUndefined();
  expect(read).toHaveBeenCalledTimes(4);
  expect(read.mock.calls[3]![0]).toEqual({ person_id: "p3", limit: 1, photo_summary: true });
  expect(changed).toHaveBeenCalledOnce();
  people.dispose();
});

it("refreshes a stale rename and reconciles a completed uncertain rename by ID", async () => {
  const renamed = { ...ada, revision: 3, display_name: "Ada Lovelace" };
  vi.spyOn(api, "listPeople").mockImplementation(byID([renamed]));
  const rename = vi
    .spyOn(api, "renamePerson")
    .mockRejectedValueOnce(new APIError("Stale revision", 412, "stale_revision"))
    .mockRejectedValueOnce(new Error("Response unavailable"));
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  expect(await people.rename(ada, "Ada Lovelace")).toBe(false);
  expect(people.writeError).toBe("Stale revision");
  expect(people.items[0]).toEqual(renamed);
  expect(await people.rename(renamed, "Ada Lovelace")).toBe(true);
  expect(rename.mock.calls[1]![2]).toEqual({ "If-Match": '"3"' });
  expect(people.writeError).toBe("");
  people.dispose();
});

it("removes the absorbed card and refreshes only the survivor after a merge", async () => {
  const kept = { ...grace, revision: 5, photo_summary: { library_count: 3 } };
  const read = vi.spyOn(api, "listPeople").mockImplementation(byID([kept]));
  vi.spyOn(api, "mergePerson").mockResolvedValue({} as api.PersonMergeReceipt);
  const people = new People("session", vi.fn());
  people.items = [ada, person("between"), grace];
  merging(people, ada, grace);
  expect(await people.submitMerge()).toBe(true);
  expect(people.items).toEqual([person("between"), kept]);
  expect(people.merge).toBeUndefined();
  expect(read).toHaveBeenCalledOnce();
  people.dispose();
});

it("retries an uncertain merge with the identical request", async () => {
  vi.spyOn(api, "listPeople").mockImplementation(byID([grace]));
  const merge = vi
    .spyOn(api, "mergePerson")
    .mockRejectedValueOnce(new Error("Response unavailable"))
    .mockResolvedValue({} as api.PersonMergeReceipt);
  const people = new People("session", vi.fn());
  merging(people, ada, grace);
  expect(await people.submitMerge()).toBe(false);
  expect(people.merge?.notice).toBe("uncertain");
  expect(people.merge?.confirmed).toBe(true);
  expect(await people.submitMerge()).toBe(true);
  const [id, request, headers] = merge.mock.calls[0]!;
  expect(id).toBe("grace");
  expect(request).toMatchObject({ absorbed_person_id: "ada", absorbed_revision: 2 });
  expect(headers).toEqual({ "If-Match": '"4"' });
  expect(merge.mock.calls[1]).toEqual([id, request, headers, expect.anything()]);
  people.dispose();
});

it("uses a new operation when the survivor changes", async () => {
  const merge = vi.spyOn(api, "mergePerson").mockRejectedValue(new Error("Response unavailable"));
  const people = new People("session", vi.fn());
  merging(people, ada, grace);
  await people.submitMerge();
  people.chooseSurvivor(person("zoe"));
  people.confirmMerge(true);
  await people.submitMerge();
  expect(merge.mock.calls[0]![1].operation_id).not.toBe(merge.mock.calls[1]![1].operation_id);
  people.dispose();
});

it("shows both current names after a stale merge and asks for a fresh confirmation", async () => {
  const source = { ...ada, display_name: "Ada L", revision: 3 };
  const survivor = { ...grace, display_name: "Grace H", revision: 6 };
  vi.spyOn(api, "listPeople").mockImplementation(byID([source, survivor]));
  const merge = vi
    .spyOn(api, "mergePerson")
    .mockRejectedValueOnce(new APIError("Stale revision", 412, "stale_revision"))
    .mockResolvedValue({} as api.PersonMergeReceipt);
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  merging(people, ada, grace);
  expect(await people.submitMerge()).toBe(false);
  expect(people.merge).toMatchObject({ source, survivor, confirmed: false, notice: "stale" });
  expect(people.items[0]).toEqual(source);
  expect(await people.submitMerge()).toBe(false);
  expect(merge).toHaveBeenCalledOnce();
  people.confirmMerge(true);
  expect(await people.submitMerge()).toBe(true);
  expect(merge.mock.calls[1]![1].absorbed_revision).toBe(3);
  expect(merge.mock.calls[1]![2]).toEqual({ "If-Match": '"6"' });
  expect(merge.mock.calls[1]![1].operation_id).not.toBe(merge.mock.calls[0]![1].operation_id);
  people.dispose();
});

it("keeps the stale merge selection when the current names can't be read", async () => {
  vi.spyOn(api, "listPeople").mockRejectedValue(new Error("Offline"));
  vi.spyOn(api, "mergePerson").mockRejectedValue(new APIError("Stale", 412, "stale_revision"));
  const people = new People("session", vi.fn());
  people.items = [ada, grace];
  merging(people, ada, grace);
  expect(await people.submitMerge()).toBe(false);
  expect(people.merge).toMatchObject({ source: ada, survivor: grace, confirmed: false });
  expect(people.items).toEqual([ada, grace]);
  expect(people.writeError).toBe("Stale");
  people.dispose();
});

it.each([
  ["survivor", { survivor: undefined, notice: "reselect" }],
  ["source", undefined],
] as const)(
  "requires a new selection when the stale merge %s is no longer visible",
  async (gone, expected) => {
    vi.spyOn(api, "listPeople").mockImplementation(byID(gone === "source" ? [grace] : [ada]));
    vi.spyOn(api, "mergePerson").mockRejectedValue(new APIError("Stale", 412, "stale_revision"));
    const people = new People("session", vi.fn());
    people.items = [ada, grace];
    merging(people, ada, grace);
    expect(await people.submitMerge()).toBe(false);
    if (expected) expect(people.merge).toMatchObject(expected);
    else expect(people.merge).toBeUndefined();
    expect(people.items).toEqual(gone === "source" ? [grace] : [ada]);
    people.dispose();
  },
);

it("keeps the exact face versions of a visible merge conflict", async () => {
  vi.spyOn(api, "mergePerson").mockRejectedValue(
    new APIError(
      "Review faces; a locked Hidden photo also needs review",
      409,
      "person_merge_conflict",
      undefined,
      [],
      [
        { face_id: "face-a", version: "old-version" },
        { face_id: "face-b", version: "current-version" },
      ],
    ),
  );
  const people = new People("session", vi.fn());
  merging(people, ada, grace);
  expect(await people.submitMerge()).toBe(false);
  expect(people.merge?.conflicts).toEqual([
    { face_id: "face-a", version: "old-version" },
    { face_id: "face-b", version: "current-version" },
  ]);
  expect(people.merge?.confirmed).toBe(false);
  expect(people.writeError).toContain("locked Hidden photo");
  people.conflictCleared(people.merge!.conflicts[0]!);
  expect(people.merge?.conflicts).toEqual([{ face_id: "face-b", version: "current-version" }]);
  people.dispose();
});

it("reports sign-in expiry", async () => {
  vi.spyOn(api, "listPeople").mockRejectedValue(new APIError("Sign in", 401, "unauthorized"));
  const failed = vi.fn();
  const people = new People("session", failed);
  await people.load();
  expect(failed).toHaveBeenCalled();
  expect(people.error).toBe("Sign in");
  people.dispose();
});
