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
const face: api.PhotoFace = {
  face_id: "face-a",
  content_version_id: "version-a",
  revision: 3,
  box: { x: 0.1, y: 0.2, width: 0.3, height: 0.4 },
  created_at: "2026-01-01T00:00:00Z",
  dismissed: false,
};
it("chooses an existing person by ID and opens their photos from the person card", async () => {
  const assertion: api.PhotoPersonAssertion = {
    assertion_id: "assertion-a",
    person_id: "person-a",
    display_name: "Ada",
    content_version_id: "version-a",
    action: "assert",
    role: "depicted",
    revision: 1,
    note: "",
    recorded_at: "2026-01-01T00:00:00Z",
  };
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
