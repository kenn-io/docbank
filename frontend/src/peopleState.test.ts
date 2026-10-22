import { afterEach, expect, it, vi } from "vite-plus/test";
import { People, peoplePageSize } from "./people.svelte.js";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { HiddenAccess } from "./hiddenAccess.svelte.js";
import { ada, grace, person } from "./people-test-fixtures.js";

afterEach(() => {
  vi.restoreAllMocks();
});
const byID = (rows: api.PersonSummary[]) => (params?: api.ListPeopleParams) =>
  Promise.resolve({ items: rows.filter((row) => row.person_id === params?.person_id) });
const merging = (people: People, source: api.PersonSummary, survivor: api.PersonSummary) => {
  people.toggle(source);
  people.toggle(survivor);
  people.startMerge();
  people.keep(survivor.person_id);
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
  people.keep("ada");
  people.confirmMerge(true);
  await people.submitMerge();
  expect(merge.mock.calls[1]![0]).toBe("ada");
  expect(merge.mock.calls[1]![1].absorbed_person_id).toBe("grace");
  expect(merge.mock.calls[0]![1].operation_id).not.toBe(merge.mock.calls[1]![1].operation_id);
  people.dispose();
});

it("keeps two selections across searches and merges them", async () => {
  vi.spyOn(api, "listPeople")
    .mockResolvedValueOnce({ items: [ada] })
    .mockResolvedValueOnce({ items: [grace, person("zoe")] });
  const people = new People("session", vi.fn());
  await people.load("Ada");
  people.toggle(people.items[0]!);
  await people.load("G");
  people.toggle(people.items[0]!);
  // A third person waits until one of the two is cleared.
  people.toggle(people.items[1]!);
  expect(people.selected).toEqual([ada, grace]);
  people.startMerge();
  expect(people.merge?.pair).toEqual([ada, grace]);
  expect(people.merge?.keep).toBeUndefined();
  people.conceal();
  expect(people.selected).toEqual([]);
  expect(people.merge).toBeUndefined();
  people.dispose();
});

it("keeps a card read from before Hidden locked out of the new cards", async () => {
  const access = new HiddenAccess("session", vi.fn());
  access.acceptState({ configured: true, expires_in: 60, expires_at: "later" }, Date.now());
  const unlockedAda = { ...ada, photo_summary: { library_count: 3, hidden_count: 1 } };
  let finish!: (page: api.PersonPage) => void;
  vi.spyOn(api, "renamePerson").mockResolvedValue({
    ...unlockedAda,
    display_name: "Ada L",
    revision: 3,
    origin: "operator",
    updated_at: "2026-01-02",
  });
  const read = vi
    .spyOn(api, "listPeople")
    .mockImplementationOnce(() => new Promise((resolve) => (finish = resolve)))
    .mockResolvedValueOnce({ items: [{ ...ada, photo_summary: { library_count: 3 } }] });
  const people = new People("session", vi.fn(), access);
  people.items = [unlockedAda];
  people.toggle(unlockedAda);
  const pending = people.rename(unlockedAda, "Ada L");
  await vi.waitFor(() => expect(read).toHaveBeenCalledOnce());
  // Hidden locks: the cards reload without the Hidden count.
  people.conceal();
  access.acceptState({ configured: true }, Date.now() + 1);
  const reload = people.load();
  finish({ items: [{ ...unlockedAda, display_name: "Ada L", revision: 3 }] });
  await Promise.all([pending, reload]);
  expect(people.items).toEqual([{ ...ada, photo_summary: { library_count: 3 } }]);
  expect(people.selected).toEqual([]);
  people.dispose();
  access.dispose();
});

it.each(["refreshed", "offline", "survivor missing", "source missing"])(
  "reconciles a stale merge when its refresh is %s",
  async (outcome) => {
    const source = { ...ada, display_name: "Ada L", revision: 3 };
    const survivor = { ...grace, display_name: "Grace H", revision: 6 };
    const kept = outcome === "source missing" ? grace : ada;
    const read = vi.spyOn(api, "listPeople");
    if (outcome === "offline") read.mockRejectedValue(new Error("Offline"));
    else read.mockImplementation(byID(outcome === "refreshed" ? [source, survivor] : [kept]));
    const merge = vi
      .spyOn(api, "mergePerson")
      .mockRejectedValueOnce(new APIError("Stale revision", 412, "stale_revision"))
      .mockResolvedValue({} as api.PersonMergeReceipt);
    const people = new People("session", vi.fn());
    people.items = [ada, grace];
    merging(people, ada, grace);
    expect(await people.submitMerge()).toBe(false);
    if (outcome === "refreshed") {
      expect(people.merge).toMatchObject({
        pair: [source, survivor],
        keep: "grace",
        confirmed: false,
        notice: "stale",
      });
      expect(people.items[0]).toEqual(source);
      expect(await people.submitMerge()).toBe(false);
      expect(merge).toHaveBeenCalledOnce();
      people.confirmMerge(true);
      expect(await people.submitMerge()).toBe(true);
      expect(merge.mock.calls[1]![1].absorbed_revision).toBe(3);
      expect(merge.mock.calls[1]![2]).toEqual({ "If-Match": '"6"' });
      expect(merge.mock.calls[1]![1].operation_id).not.toBe(merge.mock.calls[0]![1].operation_id);
    } else if (outcome === "offline") {
      expect(people.merge).toMatchObject({ pair: [ada, grace], confirmed: false, notice: "" });
      expect(people.items).toEqual([ada, grace]);
      expect(people.writeError).toBe("Stale revision");
    } else {
      expect(people.merge).toBeUndefined();
      expect(people.items).toEqual([kept]);
      expect(people.selected).toEqual([kept]);
      expect(people.writeError).toContain("no longer available");
    }
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

it("keeps renamed cards unique when they cross the saved cursor", async () => {
  const renamed = { ...ada, display_name: "Zoe", revision: 3 };
  vi.spyOn(api, "listPeople")
    .mockResolvedValueOnce({ items: [ada], next_cursor: "next" })
    .mockResolvedValueOnce({ items: [renamed] })
    .mockResolvedValueOnce({ items: [grace, renamed] });
  vi.spyOn(api, "renamePerson").mockResolvedValue({
    ...renamed,
    origin: "operator",
    updated_at: "2026-01-02",
  });
  const people = new People("session", vi.fn());
  await people.load();
  await people.rename(ada, "Zoe");
  await people.load("", true);
  expect(people.items.map((row) => row.person_id)).toEqual(["ada", "grace"]);
  people.dispose();
});

it.each(["write failure", "stale refresh"])(
  "keeps merge names concealed after a late %s",
  async (outcome) => {
    let reject!: (cause: Error) => void;
    const finish: ((page: api.PersonPage) => void)[] = [];
    const merge = vi.spyOn(api, "mergePerson");
    if (outcome === "write failure") {
      merge.mockImplementation(() => new Promise((_, fail) => (reject = fail)));
    } else {
      merge.mockRejectedValue(new APIError("Stale", 412, "stale_revision"));
      vi.spyOn(api, "listPeople").mockImplementation(
        () => new Promise((resolve) => finish.push(resolve)),
      );
    }
    const people = new People("session", vi.fn());
    merging(people, ada, grace);
    const pending = people.submitMerge();
    if (outcome === "stale refresh") {
      await vi.waitFor(() => expect(finish).toHaveLength(2));
    }
    people.conceal();
    expect(people.merge).toBeUndefined();
    if (outcome === "write failure") reject(new Error("Offline"));
    else {
      finish[0]!({ items: [ada] });
      finish[1]!({ items: [grace] });
    }
    await pending;
    expect(people.merge).toBeUndefined();
    expect(people.busy).toBe(false);
    people.dispose();
  },
);
