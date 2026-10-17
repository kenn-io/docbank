import {
  listPhotoFaces,
  createPhotoFace,
  decidePhotoFace,
  type PhotoFace,
  type PhotoPersonAssertion,
  type FaceBox,
  type PhotoFacePerson,
} from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { randomUUID } from "./crypto.js";

export class PhotoFaces {
  faces = $state.raw<PhotoFace[]>([]);
  assertions = $state.raw<PhotoPersonAssertion[]>([]);
  people = $state.raw<PhotoFacePerson[]>([]);
  version = $state("");
  loading = $state(false);
  busy = $state(false);
  error = $state("");
  pending = $state.raw<{ version: string; box: FaceBox; face_id: string }>();
  private controller?: AbortController;
  private writer?: AbortController;
  constructor(
    private session: string,
    private onauthfailure: (cause: unknown) => void,
    private hidden = false,
    private ondecision: () => void = () => {},
  ) {}
  person(face: PhotoFace) {
    return this.assertions.find(
      (row) => row.assertion_id === face.assertion_id && row.action === "assert",
    );
  }
  label(face: PhotoFace) {
    const person = this.person(face);
    return person ? this.personLabel(person.person_id) : "Unnamed face";
  }
  personLabel(id: string) {
    return this.people.find((person) => person.person_id === id)?.display_name ?? "Unnamed person";
  }
  async load(version: string) {
    this.controller?.abort();
    const changed = version !== this.version;
    if (changed) {
      this.writer?.abort();
      this.pending = undefined;
    }
    const controller = (this.controller = new AbortController());
    this.version = version;
    if (changed || !version) {
      this.faces = [];
      this.assertions = [];
      this.people = [];
    }
    this.error = "";
    this.loading = !!version;
    this.busy = false;
    if (!version) return;
    try {
      const page = await listPhotoFaces(version, {
        session: this.session,
        signal: controller.signal,
      });
      if (!controller.signal.aborted) {
        this.faces = page.faces;
        this.assertions = page.assertions;
        this.people = page.people;
        if (this.pending && page.faces.some((face) => face.face_id === this.pending?.face_id))
          this.pending = undefined;
      }
    } catch (cause) {
      if (!controller.signal.aborted) this.failed(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }
  private failed(cause: unknown) {
    this.error = cause instanceof Error ? cause.message : String(cause);
    if (
      cause instanceof APIError &&
      (cause.status === 401 || (this.hidden && cause.status === 403))
    )
      this.onauthfailure(cause);
  }
  private async write(
    action: (version: string, signal: AbortSignal) => Promise<unknown>,
    decision = false,
  ) {
    if (this.loading || this.busy || !this.version) return false;
    const version = this.version,
      controller = (this.writer = new AbortController());
    this.busy = true;
    this.error = "";
    try {
      await action(version, controller.signal);
      if (controller.signal.aborted) return false;
      if (decision) this.ondecision();
      if (this.version === version) await this.load(version);
      return true;
    } catch (cause) {
      if (!controller.signal.aborted) {
        if (cause instanceof APIError && (cause.status === 409 || cause.status === 412))
          await this.load(version);
        this.failed(cause);
      }
      return false;
    } finally {
      if (!controller.signal.aborted) this.busy = false;
    }
  }
  name(face: PhotoFace, personID: string) {
    return this.write(
      (version, signal) =>
        decidePhotoFace(
          version,
          {
            action: "name",
            face_id: face.face_id,
            revision: face.revision,
            person_id: personID,
            operation_id: randomUUID(),
          },
          { session: this.session, signal },
        ),
      true,
    );
  }
  clear(face: PhotoFace) {
    return this.write(
      (version, signal) =>
        decidePhotoFace(
          version,
          {
            action: "retract",
            face_id: face.face_id,
            revision: face.revision,
            operation_id: randomUUID(),
          },
          { session: this.session, signal },
        ),
      true,
    );
  }
  async add(box: FaceBox) {
    if (this.pending || this.loading || this.busy || !this.version) return false;
    this.pending = { version: this.version, box: { ...box }, face_id: randomUUID() };
    return this.retryAdd();
  }
  async retryAdd() {
    const pending = this.pending;
    if (!pending || this.loading || this.busy || pending.version !== this.version) return false;
    const controller = (this.writer = new AbortController());
    this.busy = true;
    this.error = "";
    try {
      await createPhotoFace(
        pending.version,
        { box: pending.box, face_id: pending.face_id },
        { session: this.session, signal: controller.signal },
      );
      if (controller.signal.aborted) return false;
      this.pending = undefined;
      await this.load(pending.version);
      return !controller.signal.aborted;
    } catch (cause) {
      if (controller.signal.aborted) return false;
      if (cause instanceof APIError && cause.status >= 400 && cause.status < 500)
        this.pending = undefined;
      await this.load(pending.version);
      if (controller.signal.aborted) return false;
      if (!this.pending && this.faces.some((face) => face.face_id === pending.face_id)) return true;
      this.failed(cause);
      return false;
    } finally {
      if (!controller.signal.aborted) this.busy = false;
    }
  }
  dispose() {
    this.controller?.abort();
    this.writer?.abort();
  }
}
