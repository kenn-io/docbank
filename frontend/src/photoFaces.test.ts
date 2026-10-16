import { afterEach, expect, it, vi } from "vite-plus/test";
import { PhotoFaces } from "./photoFaces.svelte.js";
import { drawnPhotoFace, rotatePhotoBox } from "./photoRotation.js";
import * as api from "./generated/docbank.js";

afterEach(() => vi.restoreAllMocks());
const face: api.PhotoFace = {
  face_id: "face-a",
  content_version_id: "version-a",
  box: { x: 0.1, y: 0.2, width: 0.3, height: 0.4 },
  revision: 7,
  assertion_id: "assertion-a",
  created_at: "2026-01-01T00:00:00Z",
  dismissed: false,
};
const assertion: api.PhotoPersonAssertion = {
  assertion_id: "assertion-a",
  content_version_id: "version-a",
  person_id: "person-a",
  display_name: "Ada",
  action: "assert",
  role: "depicted",
  revision: 1,
  recorded_at: "2026-01-01T00:00:00Z",
  note: "",
};
it("names the face with its current revision and refreshes its active label", async () => {
  const read = vi
    .spyOn(api, "listPhotoFaces")
    .mockResolvedValueOnce({ faces: [face], assertions: [] })
    .mockResolvedValue({ faces: [{ ...face, revision: 8 }], assertions: [assertion] });
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
    .mockResolvedValueOnce({ faces: [face], assertions: [assertion] })
    .mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          finish = resolve;
        }),
    )
    .mockResolvedValue({ faces: [], assertions: [] });
  const faces = new PhotoFaces("session", vi.fn());
  await faces.load("version-a");
  const pending = faces.load("version-b");
  expect(faces.faces).toEqual([]);
  expect(faces.assertions).toEqual([]);
  await faces.load("version-c");
  finish({ faces: [face], assertions: [assertion] });
  await pending;
  expect(faces.version).toBe("version-c");
  expect(faces.faces).toEqual([]);
  expect(read.mock.calls[1]![1]!.signal!.aborted).toBe(true);
  faces.dispose();
});
it.each([
  [0, { x: 0.1, y: 0.2, width: 0.3, height: 0.4 }],
  [90, { x: 0.4, y: 0.1, width: 0.4, height: 0.3 }],
  [180, { x: 0.6, y: 0.4, width: 0.3, height: 0.4 }],
  [270, { x: 0.2, y: 0.6, width: 0.4, height: 0.3 }],
])("rotates face corners and inverts a drawn region at %i degrees", (rotation, expected) => {
  const displayed = rotatePhotoBox(face.box, rotation as number);
  for (const field of ["x", "y", "width", "height"] as const)
    expect(displayed[field]).toBeCloseTo(expected[field]);
  const stored = drawnPhotoFace(
    { ...displayed, x: displayed.x + displayed.width / 2, y: displayed.y + displayed.height / 2 },
    rotation as number,
  );
  for (const field of ["x", "y", "width", "height"] as const)
    expect(stored[field]).toBeCloseTo(face.box[field]);
});
it("creates the manually drawn face on the loaded version", async () => {
  vi.spyOn(api, "listPhotoFaces").mockResolvedValue({ faces: [], assertions: [] });
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
