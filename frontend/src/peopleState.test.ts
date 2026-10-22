import { afterEach, expect, it, vi } from "vite-plus/test";
import { People, recentPeople, recordRecentPerson, clearRecentPeople } from "./people.svelte.js";
import { PhotoFaces } from "./photoFaces.svelte.js";
import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { photoFace } from "./photo-test-fixtures.js";

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
  vi.restoreAllMocks();
  clearRecentPeople();
});
it("keeps 20 actual naming choices for the signed-in session", () => {
  for (let i = 0; i < 25; i++) recordRecentPerson("session", String(i));
  recordRecentPerson("session", "20");
  expect(recentPeople("session")).toEqual([
    "20",
    "24",
    "23",
    "22",
    "21",
    "19",
    "18",
    "17",
    "16",
    "15",
    "14",
    "13",
    "12",
    "11",
    "10",
    "9",
    "8",
    "7",
    "6",
    "5",
  ]);
  expect(recentPeople("other-session")).toEqual([]);
});
it("records the new person returned by naming even if the following read fails", async () => {
  vi.spyOn(api, "listPhotoFaces")
    .mockResolvedValueOnce({ faces: [photoFace()], people: [], assertions: [] })
    .mockRejectedValue(new Error("Read failed"));
  vi.spyOn(api, "decidePhotoFace").mockResolvedValue({
    receipt_id: "receipt",
    person_id: "new-person",
  });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version");
  expect(await faces.name(photoFace(), "", "Zoe")).toBe(true);
  expect(recentPeople("session")).toEqual(["new-person"]);
  faces.dispose();
});
it("records successful whole-photo marks, and leaves rejected writes out of recency", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [], assertions: [], people: [] });
  vi.spyOn(api, "createPhotoFace")
    .mockRejectedValueOnce(new Error("Failed"))
    .mockResolvedValue({
      assertion: {
        assertion_id: "mark",
        person_id: "assigned-person",
        content_version_id: "v",
        action: "assert",
        role: "depicted",
        revision: 1,
        note: "",
        recorded_at: "2026-01-01",
      },
    });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version");
  expect(await faces.mark("", "Zoe")).toBe(false);
  expect(recentPeople("session")).toEqual([]);
  expect(await faces.mark("", "Zoe")).toBe(true);
  expect(recentPeople("session")).toEqual(["assigned-person"]);
  faces.dispose();
});
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
    limit: 100,
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
it("merges into the selected survivor with both revisions and reuses an uncertain operation", async () => {
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada, grace] });
  const merge = vi
    .spyOn(api, "mergePerson")
    .mockRejectedValueOnce(new Error("Response unavailable"))
    .mockResolvedValue({} as api.PersonMergeReceipt);
  const people = new People("session", vi.fn());
  expect(await people.merge(ada, grace)).toBe(false);
  expect(people.uncertain).toBe(true);
  expect(await people.merge(ada, grace)).toBe(true);
  const [id, request, headers] = merge.mock.calls[0]!;
  expect(id).toBe("grace");
  expect(request).toMatchObject({ absorbed_person_id: "ada", absorbed_revision: 2 });
  expect(headers).toEqual({ "If-Match": '"4"' });
  expect(merge.mock.calls[1]![1]).toEqual(request);
  people.dispose();
});
it("uses a new operation when the survivor changes", async () => {
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada, grace] });
  const merge = vi.spyOn(api, "mergePerson").mockRejectedValue(new Error("Response unavailable"));
  const people = new People("session", vi.fn());
  await people.merge(ada, grace);
  await people.merge(ada, { ...grace, person_id: "zoe" });
  expect(merge.mock.calls[0]![1].operation_id).not.toBe(merge.mock.calls[1]![1].operation_id);
  people.dispose();
});
it.each([409, 412, 422])("shows a rejected merge (%s) and refreshes the people", async (status) => {
  const read = vi.spyOn(api, "listPeople").mockResolvedValue({ items: [ada, grace] });
  vi.spyOn(api, "mergePerson").mockRejectedValue(
    new APIError("Cannot merge", status, "merge_conflict"),
  );
  const people = new People("session", vi.fn());
  expect(await people.merge(ada, grace)).toBe(false);
  expect(people.writeError).toBe("Cannot merge");
  expect(people.uncertain).toBe(false);
  expect(read).toHaveBeenCalled();
  people.dispose();
});
it("refreshes a stale rename and reconciles a completed uncertain rename", async () => {
  const renamed = { ...ada, revision: 3, display_name: "Ada Lovelace" };
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [renamed] });
  const rename = vi
    .spyOn(api, "renamePerson")
    .mockRejectedValueOnce(new APIError("Stale revision", 412, "stale_revision"))
    .mockRejectedValueOnce(new Error("Response unavailable"));
  const people = new People("session", vi.fn());
  expect(await people.rename(ada, "Ada Lovelace")).toBe(false);
  expect(people.writeError).toBe("Stale revision");
  expect(people.items[0]!.revision).toBe(3);
  expect(await people.rename(renamed, "Ada Lovelace")).toBe(true);
  expect(rename.mock.calls[1]![2]).toEqual({ "If-Match": '"3"' });
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

it("reconciles an uncertain rename after it moves outside the current search", async () => {
  const read = vi
    .spyOn(api, "listPeople")
    .mockResolvedValueOnce({ items: [] })
    .mockResolvedValueOnce({ items: [{ ...ada, display_name: "Zoe" }] });
  vi.spyOn(api, "renamePerson").mockRejectedValue(new Error("Response unavailable"));
  const people = new People("session", vi.fn());
  people.query = "Ada";
  expect(await people.rename(ada, "Zoe")).toBe(true);
  expect(read.mock.calls[1]![0]).toEqual({ query: "Zoe", limit: 100 });
  expect(people.items).toEqual([]);
  people.dispose();
});
