import { photoFace, photoPersonAssertion, photoFacePage } from "./photo-test-fixtures.js";
import { afterEach, beforeEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import { PhotoFaces } from "./photoFaces.svelte.js";
import PhotoPeople from "./PhotoPeople.svelte";
import * as api from "./generated/docbank.js";
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});
beforeEach(() =>
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      disconnect() {}
    },
  ),
);
const face = photoFace({ revision: 3, assertion_id: undefined });
it("chooses an existing person by ID and opens their photos from the person card", async () => {
  const assertion = photoPersonAssertion();
  vi.spyOn(api, "listPhotoFaces")
    .mockResolvedValueOnce({ faces: [face], assertions: [], people: [] })
    .mockResolvedValue({
      faces: [{ ...face, assertion_id: "assertion-a", revision: 4 }],
      assertions: [assertion],
      people: [{ person_id: "person-a", display_name: "Ada", state: "curated" }],
    });
  const search = vi.spyOn(api, "listPeople").mockResolvedValue({
    items: [{ person_id: "person-a", display_name: "Ada", revision: 1, state: "curated" }],
  });
  vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt-a" });
  const onperson = vi.fn(),
    faces = new PhotoFaces("session", vi.fn());
  const name = vi.spyOn(faces, "name");
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session", onperson });
  await fireEvent.click(
    screen.getByRole("button", { name: "Find or create a person: Unnamed face" }),
  );
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "雪".repeat(67) } });
  await screen.findByText("Name must be 200 bytes or fewer.");
  expect(search.mock.calls.some(([params]) => params?.query === "雪".repeat(67))).toBe(false);
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Ad" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Ada" }));
  await waitFor(() => expect(name).toHaveBeenCalledWith(face, "person-a"));
  await fireEvent.click(await screen.findByRole("button", { name: "Ada" }));
  expect(onperson).toHaveBeenCalledWith("person-a", "Ada");
  expect(search).toHaveBeenCalledWith(
    { query: "Ad", limit: 100 },
    expect.objectContaining({ session: "session" }),
  );
  search.mockResolvedValue({ items: [] });
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Ada" }));
  await waitFor(() =>
    expect(search).toHaveBeenLastCalledWith(
      { query: "", limit: 100 },
      expect.objectContaining({ session: "session" }),
    ),
  );
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Ada" }));
  await waitFor(() => expect(screen.queryByRole("combobox")).toBeNull());
  expect(name).toHaveBeenCalledTimes(1);
  faces.dispose();
});
it("searches again after a failed assignment and disables the picker throughout creation", async () => {
  const read = vi
    .spyOn(api, "listPhotoFaces")
    .mockResolvedValue({ faces: [face], assertions: [], people: [] });
  let canonical = "";
  vi.spyOn(api, "listPeople").mockImplementation(async ({ query } = {}) => ({
    items:
      canonical && canonical.toLowerCase() === query?.trim().toLowerCase()
        ? [{ person_id: "person-new", display_name: canonical, state: "curated", revision: 1 }]
        : [],
  }));
  let fail!: (error: Error) => void;
  const write = vi
    .spyOn(api, "decidePhotoFace")
    .mockImplementationOnce(
      () =>
        new Promise((_resolve, reject) => {
          fail = reject;
        }),
    )
    .mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(
    screen.getByRole("button", { name: "Find or create a person: Unnamed face" }),
  );
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Ada" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create person "Ada"' }));
  await waitFor(() => expect(write).toHaveBeenCalledOnce());
  expect((screen.getByRole("combobox") as HTMLInputElement).disabled).toBe(true);
  canonical = "Ada";
  read.mockResolvedValue({
    faces: [{ ...face, revision: 4, assertion_id: "assertion" }],
    assertions: [photoPersonAssertion({ assertion_id: "assertion", person_id: "person-new" })],
    people: [{ person_id: "person-new", display_name: "Ada", state: "curated" }],
  });
  fail(new Error("Response lost"));
  await screen.findByText("Response lost");
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "ada" } });
  await screen.findByRole("option", { name: 'Create person "ada"' });
  await fireEvent.mouseDown(screen.getByRole("option", { name: "Ada" }));
  await waitFor(() => expect(screen.queryByRole("combobox")).toBeNull());
  expect(write).toHaveBeenCalledTimes(1);
  faces.dispose();
});
it("replaces a cached selected name when another face's decision reloads canonical names", async () => {
  const first = photoFace({ assertion_id: undefined });
  const second = photoFace({ face_id: "face-b", assertion_id: undefined });
  vi.spyOn(api, "listPhotoFaces")
    .mockResolvedValueOnce(photoFacePage({ faces: [first, second], assertions: [], people: [] }))
    .mockResolvedValueOnce(photoFacePage({ faces: [photoFace(), second] }))
    .mockResolvedValue(
      photoFacePage({
        faces: [photoFace(), { ...second, assertion_id: "assertion-b" }],
        assertions: [
          photoPersonAssertion(),
          photoPersonAssertion({ assertion_id: "assertion-b", person_id: "person-b" }),
        ],
        people: [
          { person_id: "person-a", display_name: "Ada Updated", state: "curated" },
          { person_id: "person-b", display_name: "Grace", state: "curated" },
        ],
      }),
    );
  vi.spyOn(api, "listPeople").mockImplementation(async ({ query } = {}) => ({
    items: [
      {
        person_id: query === "Ada" ? "person-a" : "person-b",
        display_name: query!,
        state: "curated",
        revision: 1,
      },
    ],
  }));
  vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session", onperson: vi.fn() });
  await fireEvent.click(
    screen.getAllByRole("button", { name: "Find or create a person: Unnamed face" })[0]!,
  );
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Ada" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Ada" }));
  await screen.findByRole("button", { name: "Find or create a person: Ada" });
  await fireEvent.click(
    screen.getByRole("button", { name: "Find or create a person: Unnamed face" }),
  );
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Grace" } });
  await screen.findByRole("option", { name: "Grace" });
  expect(screen.getByRole("button", { name: "Find or create a person: Ada" })).toBeTruthy();
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Grace" }));
  await screen.findByRole("button", { name: "Find or create a person: Ada Updated" });
  expect(screen.getByRole("button", { name: "Ada Updated" })).toBeTruthy();
  await screen.findByRole("button", { name: "Find or create a person: Grace" });
  faces.dispose();
});

it("retries an empty failed face page", async () => {
  vi.spyOn(api, "listPhotoFaces")
    .mockRejectedValueOnce(new Error("Offline"))
    .mockResolvedValue(photoFacePage());
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  await screen.findByRole("button", { name: "Find or create a person: Ada" });
  faces.dispose();
});

it("keeps retired labels and correction controls without Library links", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue(
    photoFacePage({
      assertions: [photoPersonAssertion({ photo_mark: true })],
      people: [{ person_id: "person-a", display_name: "Ada", state: "retired" }],
    }),
  );
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session", onperson: vi.fn() });
  expect(screen.queryByRole("button", { name: "Ada" })).toBeNull();
  expect(screen.getByRole("button", { name: "Remove face" })).toBeTruthy();
  expect(screen.getByRole("button", { name: "Clear whole-photo mark" })).toBeTruthy();
  faces.dispose();
});
