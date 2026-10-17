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
  write.mockRejectedValueOnce(new APIError("Changed", 412, "stale_revision"));
  read.mockResolvedValue({
    faces: [{ ...face, revision: 9 }],
    assertions: [assertion],
    people: [{ person_id: "person-a", display_name: "Ada" }],
  });
  expect(await faces.name(faces.faces[0]!, "person-a")).toBe(false);
  expect(write).toHaveBeenLastCalledWith(
    "version-a",
    expect.objectContaining({ revision: 8 }),
    expect.anything(),
  );
  expect(faces.faces[0]!.revision).toBe(9);
  expect(read).toHaveBeenCalledTimes(3);
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

it.each([new Error("Response lost"), new APIError("Unavailable", 503, "unavailable")])(
  "replays an uncertain box with its original ID: %s",
  async (cause) => {
    vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [], assertions: [], people: [] });
    const create = vi
      .spyOn(api, "createPhotoFace")
      .mockRejectedValueOnce(cause)
      .mockResolvedValue({ face });
    const faces = new PhotoFaces("session", vi.fn());
    await faces.load("version-a");
    const box = { x: 0.2, y: 0.3, width: 0.1, height: 0.15 };
    expect(await faces.add(box)).toBe(false);
    const request = create.mock.calls[0]!;
    expect(create).toHaveBeenCalledWith(
      "version-a",
      { box, face_id: expect.any(String) },
      expect.objectContaining({ session: "session" }),
    );
    expect(await faces.add({ ...box, x: 0.7 })).toBe(false);
    expect(create).toHaveBeenCalledTimes(1);
    expect(await faces.retryAdd()).toBe(true);
    expect(create.mock.calls[1]!.slice(0, 2)).toEqual(request.slice(0, 2));
    expect(request[1]!.box).toEqual(box);
    expect(await faces.add(box)).toBe(true);
    expect(create.mock.calls[2]![1]!.face_id).not.toBe(request[1]!.face_id);
    faces.dispose();
  },
);
it("clears a pending create after refusal, navigation, or reconciliation", async () => {
  for (const reason of ["definitive", "version", "reconciled"]) {
    const read = vi
      .spyOn(api, "listPhotoFaces")
      .mockResolvedValue({ faces: [], assertions: [], people: [] });
    const create = vi
      .spyOn(api, "createPhotoFace")
      .mockRejectedValue(
        reason === "definitive"
          ? new APIError("Invalid", 400, "invalid")
          : new Error("Response lost"),
      );
    const faces = new PhotoFaces("session", vi.fn());
    await faces.load("version-a");
    await faces.add({ x: 0.2, y: 0.3, width: 0.1, height: 0.15 });
    if (reason === "version") await faces.load("version-b");
    if (reason === "reconciled") {
      read.mockResolvedValue({
        faces: [{ ...face, face_id: create.mock.calls[0]![1]!.face_id! }],
        assertions: [],
        people: [],
      });
      await faces.load("version-a");
    }
    expect(await faces.retryAdd()).toBe(false);
    expect(create).toHaveBeenCalledTimes(1);
    faces.dispose();
    vi.restoreAllMocks();
  }
});
