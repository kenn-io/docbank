import {
  listPhotoFaces,
  createPhotoFace,
  decidePhotoFace,
  type PhotoFace,
  type PhotoPersonAssertion,
  type FaceBox,
} from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { randomUUID } from "./crypto.js";

export class PhotoFaces {
  faces = $state.raw<PhotoFace[]>([]);
  assertions = $state.raw<PhotoPersonAssertion[]>([]);
  version = $state("");
  loading = $state(false);
  busy = $state(false);
  error = $state("");
  private controller?: AbortController;
  constructor(
    private session: string,
    private onauthfailure: (cause: unknown) => void,
  ) {}
  person(face: PhotoFace) {
    return this.assertions.find(
      (row) => row.assertion_id === face.assertion_id && row.action === "assert",
    );
  }
  label(face: PhotoFace) {
    return this.person(face)?.display_name || "Unnamed face";
  }
  async load(version: string) {
    this.controller?.abort();
    const controller = (this.controller = new AbortController());
    this.version = version;
    this.faces = [];
    this.assertions = [];
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
      }
    } catch (cause) {
      if (!controller.signal.aborted) this.failed(cause);
    } finally {
      if (!controller.signal.aborted) this.loading = false;
    }
  }
  private failed(cause: unknown) {
    this.error = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && (cause.status === 401 || cause.status === 403))
      this.onauthfailure(cause);
  }
  private async write(action: (version: string, signal: AbortSignal) => Promise<unknown>) {
    if (this.loading || this.busy || !this.version) return false;
    const version = this.version,
      controller = this.controller!;
    this.busy = true;
    this.error = "";
    try {
      await action(version, controller.signal);
      if (controller.signal.aborted) return false;
      await this.load(version);
      return !this.error;
    } catch (cause) {
      if (!controller.signal.aborted) this.failed(cause);
      return false;
    } finally {
      if (!controller.signal.aborted) this.busy = false;
    }
  }
  name(face: PhotoFace, personID: string) {
    return this.write((version, signal) =>
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
    );
  }
  add(box: FaceBox) {
    return this.write((version, signal) =>
      createPhotoFace(version, { box, face_id: randomUUID() }, { session: this.session, signal }),
    );
  }
  dispose() {
    this.controller?.abort();
  }
}
