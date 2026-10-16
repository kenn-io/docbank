import { photoFace, photoPersonAssertion } from "./photo-test-fixtures.js";
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
    .mockResolvedValueOnce({ faces: [face], assertions: [] })
    .mockResolvedValue({
      faces: [{ ...face, assertion_id: "assertion-a", revision: 4 }],
      assertions: [assertion],
    });
  const search = vi.spyOn(api, "listPeople").mockResolvedValue({
    items: [{ person_id: "person-a", display_name: "Ada", revision: 1, state: "curated" }],
  });
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt-a" });
  const onperson = vi.fn(),
    faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session", onperson });
  await fireEvent.click(
    screen.getByRole("button", { name: "Find or create a person: Unnamed face" }),
  );
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Ad" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Ada" }));
  await waitFor(() =>
    expect(write).toHaveBeenCalledWith(
      "version-a",
      expect.objectContaining({ face_id: "face-a", revision: 3, person_id: "person-a" }),
      expect.anything(),
    ),
  );
  await fireEvent.click(await screen.findByRole("button", { name: "Ada" }));
  expect(onperson).toHaveBeenCalledWith("person-a");
  expect(search).toHaveBeenCalledWith(
    { query: "Ad", limit: 100 },
    expect.objectContaining({ session: "session" }),
  );
  faces.dispose();
});
it("creates a person only after choosing Create", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [face], assertions: [] });
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [] });
  const create = vi.spyOn(api, "createPerson").mockResolvedValue({
    person_id: "person-new",
    display_name: "Grace",
    origin: "operator",
    state: "curated",
    revision: 1,
    created_at: "",
    updated_at: "",
  });
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt-a" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(
    screen.getByRole("button", { name: "Find or create a person: Unnamed face" }),
  );
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Grace" } });
  const choice = await screen.findByRole("option", { name: 'Create person "Grace"' });
  expect(create).not.toHaveBeenCalled();
  await fireEvent.mouseDown(choice);
  await waitFor(() =>
    expect(write).toHaveBeenCalledWith(
      "version-a",
      expect.objectContaining({ person_id: "person-new" }),
      expect.anything(),
    ),
  );
  expect(create).toHaveBeenCalledWith(
    { display_name: "Grace" },
    expect.objectContaining({ session: "session" }),
  );
  faces.dispose();
});

it("keeps a created person for retry and disables the picker throughout creation", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [face], assertions: [] });
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [] });
  let finish!: (person: api.Person) => void;
  const create = vi.spyOn(api, "createPerson").mockImplementation(
    () =>
      new Promise((resolve) => {
        finish = resolve;
      }),
  );
  const write = vi
    .spyOn(api, "decidePhotoFace")
    .mockRejectedValueOnce(new Error("Try again"))
    .mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  const component = render(PhotoPeople, { faces, session: "session" });
  await fireEvent.click(
    screen.getByRole("button", { name: "Find or create a person: Unnamed face" }),
  );
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Grace" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create person "Grace"' }));
  await waitFor(() => expect(create).toHaveBeenCalledTimes(1));
  expect(
    (component.container.querySelector(".kit-typeahead__input") as HTMLInputElement).disabled,
  ).toBe(true);
  finish({
    person_id: "person-new",
    display_name: "Grace",
    origin: "operator",
    state: "curated",
    revision: 1,
    created_at: "",
    updated_at: "",
  });
  await screen.findByText("Try again");
  await fireEvent.input(screen.getByRole("combobox"), { target: { value: "Grace" } });
  await fireEvent.mouseDown(await screen.findByRole("option", { name: 'Create person "Grace"' }));
  await waitFor(() => expect(write).toHaveBeenCalledTimes(2));
  expect(create).toHaveBeenCalledTimes(1);
  expect(write.mock.calls[1]![1].person_id).toBe("person-new");
  expect(create.mock.calls[0]![1]!.signal!.aborted).toBe(false);
  faces.dispose();
});
it("offers Clear name for named faces and keeps whole-photo marks as person links", async () => {
  const assertion = photoPersonAssertion();
  vi.spyOn(api, "listPhotoFaces")
    .mockResolvedValueOnce({
      faces: [{ ...face, assertion_id: assertion.assertion_id }],
      assertions: [assertion, photoPersonAssertion({ assertion_id: "whole", photo_mark: true })],
    })
    .mockResolvedValue({
      faces: [face],
      assertions: [photoPersonAssertion({ assertion_id: "whole", photo_mark: true })],
    });
  vi.spyOn(api, "listPeople").mockResolvedValue({ items: [] });
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  const component = render(PhotoPeople, { faces, session: "session", onperson: vi.fn() });
  expect(component.container.querySelectorAll(".kit-typeahead")).toHaveLength(1);
  await fireEvent.click(screen.getByRole("button", { name: "Find or create a person: Ada" }));
  await fireEvent.mouseDown(await screen.findByRole("option", { name: "Clear name" }));
  await waitFor(() =>
    expect(write).toHaveBeenCalledWith(
      "version-a",
      expect.objectContaining({ action: "retract", revision: 3 }),
      expect.anything(),
    ),
  );
  await waitFor(() =>
    expect(
      screen.getByRole("button", { name: "Find or create a person: Unnamed face" }),
    ).toBeTruthy(),
  );
  expect(screen.getByRole("button", { name: "Ada" })).toBeTruthy();
  faces.dispose();
});
