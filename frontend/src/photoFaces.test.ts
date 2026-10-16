import { photoFace, photoPersonAssertion, photoFacePage } from "./photo-test-fixtures.js";
import { APIError } from "./api-transport.js";
import { afterEach, expect, it, vi } from "vite-plus/test";
import { PhotoFaces } from "./photoFaces.svelte.js";
import * as api from "./generated/docbank.js";

afterEach(() => vi.restoreAllMocks());
const face = photoFace();
const assertion = photoPersonAssertion();
it("names the face with its current revision and refreshes its active label", async () => {
  const read = vi
    .spyOn(api, "listPhotoFaces")
    .mockResolvedValueOnce({ faces: [face], assertions: [], people: [] })
    .mockResolvedValue({
      faces: [{ ...face, revision: 8 }],
      assertions: [assertion],
      people: [{ person_id: "person-a", display_name: "Ada" }],
    });
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt-a" });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  expect(faces.label(face)).toBe("Unnamed face");
  expect(await faces.name(face, "person-a")).toBe(true);
  expect(write).toHaveBeenCalledWith(
    "version-a",
    expect.objectContaining({
      action: "name",
      face_id: "face-a",
      person_id: "person-a",
      revision: 7,
    }),
    expect.objectContaining({ session: "session" }),
  );
  expect(faces.label(face)).toBe("Ada");
  expect(read).toHaveBeenCalledTimes(2);
  faces.dispose();
});
it("clears rows on navigation and ignores a late old page", async () => {
  let finish!: (page: api.PhotoFacePage) => void;
  const read = vi
    .spyOn(api, "listPhotoFaces")
    .mockResolvedValueOnce(photoFacePage())
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    )
    .mockResolvedValue({ faces: [], assertions: [], people: [] });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  const pending = faces.load("version-b");
  expect(faces.faces).toEqual([]);
  expect(faces.assertions).toEqual([]);
  await faces.load("version-c");
  finish(photoFacePage());
  await pending;
  expect(faces.version).toBe("version-c");
  expect(faces.faces).toEqual([]);
  expect(read.mock.calls[1]![1]!.signal!.aborted).toBe(true);
  faces.dispose();
});
it("creates the manually drawn face on the loaded version", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [], assertions: [], people: [] });
  const write = vi.spyOn(api, "createPhotoFace").mockResolvedValue({ face });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  await faces.add({ x: 0.2, y: 0.3, width: 0.1, height: 0.1 });
  expect(write).toHaveBeenCalledWith(
    "version-a",
    expect.objectContaining({
      box: { x: 0.2, y: 0.3, width: 0.1, height: 0.1 },
      face_id: expect.any(String),
    }),
    expect.objectContaining({ session: "session" }),
  );
  faces.dispose();
});

it("notifies a successful clear before reload even when the reload fails", async () => {
  const events: string[] = [];
  vi.spyOn(api, "listPhotoFaces")
    .mockResolvedValueOnce(photoFacePage())
    .mockImplementation(async () => {
      events.push("reload");
      throw new Error("Read failed");
    });
  const write = vi.spyOn(api, "decidePhotoFace").mockResolvedValue({ receipt_id: "receipt" });
  const faces = new PhotoFaces("session", vi.fn(), false, () => events.push("notify"));
  await faces.load("version-a");
  expect(await faces.clear(face)).toBe(true);
  expect(events).toEqual(["notify", "reload"]);
  expect(write).toHaveBeenCalledWith(
    "version-a",
    expect.objectContaining({ action: "retract", face_id: face.face_id, revision: 7 }),
    expect.anything(),
  );
  expect(faces.error).toBe("Read failed");
});
it("reloads the current face revision after a conflicting write", async () => {
  const read = vi
    .spyOn(api, "listPhotoFaces")
    .mockResolvedValueOnce({ faces: [face], assertions: [], people: [] })
    .mockResolvedValue({
      faces: [{ ...face, revision: 8 }],
      assertions: [assertion],
      people: [{ person_id: "person-a", display_name: "Ada" }],
    });
  vi.spyOn(api, "decidePhotoFace").mockRejectedValue(
    new APIError("Changed", 412, "stale_revision"),
  );
  const notify = vi.fn(),
    faces = new PhotoFaces("session", vi.fn(), false, notify);
  await faces.load("version-a");
  expect(await faces.name(face, "person-a")).toBe(false);
  expect(faces.faces[0]!.revision).toBe(8);
  expect(read).toHaveBeenCalledTimes(2);
  expect(notify).not.toHaveBeenCalled();
});
it.each([
  [false, 401, true],
  [false, 403, false],
  [true, 403, true],
])("routes auth errors with hidden=%s status=%i", async (hidden, status, escalates) => {
  const cause = new APIError("Denied", status as number, "denied");
  vi.spyOn(api, "listPhotoFaces").mockRejectedValue(cause);
  const auth = vi.fn(),
    faces = new PhotoFaces("session", auth, hidden as boolean);
  await faces.load("version-a");
  expect(auth).toHaveBeenCalledTimes(escalates ? 1 : 0);
  expect(faces.error).toBe("Denied");
});
