import { photoFace, photoFacePage } from "./photo-test-fixtures.js";
import { APIError } from "./api-transport.js";
import { afterEach, expect, it, vi } from "vite-plus/test";
import { PhotoFaces } from "./photoFaces.svelte.js";
import * as api from "./generated/docbank.js";
import { HiddenAccess } from "./hiddenAccess.svelte.js";

afterEach(() => vi.restoreAllMocks());
const face = photoFace();
it.each(["create", "name", "clear"] as const)(
  "settles a Hidden %s before locking after disposal",
  async (action) => {
    const events: string[] = [];
    let finish!: () => void;
    let signal!: AbortSignal;
    vi.spyOn(api, "listPhotoFaces").mockResolvedValue(photoFacePage());
    const held = async (
      _version: string,
      _body: unknown,
      options?: { signal?: AbortSignal | null },
    ) => {
      signal = options!.signal!;
      await new Promise<void>((resolve) => (finish = resolve));
      events.push("saved");
    };
    vi.spyOn(api, "createPhotoFace").mockImplementation(async (...args) => {
      await held(...args);
      return { face };
    });
    vi.spyOn(api, "decidePhotoFace").mockImplementation(async (...args) => {
      await held(...args);
      return { receipt_id: "receipt-a" };
    });
    vi.spyOn(api, "lockPhotoHidden").mockImplementation(async () => {
      events.push("locked");
      return { configured: true };
    });
    const access = new HiddenAccess("session", vi.fn());
    const faces = new PhotoFaces("session", vi.fn(), true, vi.fn(), access);
    await faces.load("version-a");
    const saving =
      action === "create"
        ? faces.add({ x: 0.1, y: 0.2, width: 0.3, height: 0.4 })
        : action === "name"
          ? faces.name(face, "", "Ada")
          : faces.clear(face);
    await vi.waitFor(() => expect(finish).toBeTypeOf("function"));
    faces.dispose();
    const locking = access.lock();
    await Promise.resolve();
    expect(signal.aborted).toBe(false);
    expect(events).toEqual([]);
    finish();
    expect(await saving).toBe(true);
    expect(await locking).toBe(true);
    expect(events).toEqual(["saved", "locked"]);
    expect(api.listPhotoFaces).toHaveBeenCalledTimes(1);
  },
);
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

it.each(["network", "server", "definitive", "version", "reconciled"] as const)(
  "replays or clears a pending face create after %s",
  async (reason) => {
    const read = vi
      .spyOn(api, "listPhotoFaces")
      .mockResolvedValue({ faces: [], assertions: [], people: [] });
    const cause =
      reason === "definitive"
        ? new APIError("Invalid", 400, "invalid")
        : reason === "server"
          ? new APIError("Unavailable", 503, "unavailable")
          : new Error("Response lost");
    const create = vi
      .spyOn(api, "createPhotoFace")
      .mockRejectedValueOnce(cause)
      .mockResolvedValue({ face });
    const faces = new PhotoFaces("session", vi.fn());
    await faces.load("version-a");
    const box = { x: 0.2, y: 0.3, width: 0.1, height: 0.15 };
    expect(await faces.add(box)).toBe(false);
    if (reason === "network" || reason === "server") {
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
    } else {
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
    }
    faces.dispose();
  },
);

it("numbers visible faces by creation time and ID, closing gaps after dismissal", () => {
  const faces = new PhotoFaces("session", vi.fn());
  const earlier = photoFace({
    face_id: "earlier",
    created_at: "2025-01-01T00:00:00Z",
    assertion_id: undefined,
  });
  const a = photoFace({ face_id: "a", assertion_id: undefined });
  const b = photoFace({ face_id: "b", assertion_id: undefined });
  faces.faces = [b, earlier, a];
  expect(faces.ordered.map((face) => face.face_id)).toEqual(["earlier", "a", "b"]);
  expect(faces.label(b)).toBe("Face 3");
  faces.faces = [photoFace({ face_id: "new", created_at: "2026-02-01T00:00:00Z" }), ...faces.faces];
  expect(faces.label(a)).toBe("Face 2");
  faces.faces = faces.faces.map((face) =>
    face.face_id === "earlier" ? { ...face, dismissed: true } : face,
  );
  expect(faces.ordered.map((face) => face.face_id)).toEqual(["a", "b", "new"]);
  expect(faces.number(a)).toBe("Face 1");
  expect(faces.number(b)).toBe("Face 2");
  expect(faces.number(faces.ordered[2]!)).toBe("Face 3");
  const first = photoFace({ face_id: "a", dismissed: true });
  const remaining = photoFace({ face_id: "b", assertion_id: undefined });
  faces.faces = [first, remaining];
  expect(faces.label(remaining)).toBe("Face 1");
  const added = photoFace({
    face_id: "new",
    created_at: "2026-02-01T00:00:00Z",
    assertion_id: undefined,
  });
  faces.faces = [...faces.faces, added];
  expect(faces.label(remaining)).toBe("Face 1");
  expect(faces.label(added)).toBe("Face 2");
  faces.dispose();
});
