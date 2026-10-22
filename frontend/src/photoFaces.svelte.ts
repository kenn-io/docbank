import {
  listPhotoFaces,
  createPhotoFace,
  decidePhotoFace,
  type PhotoFace,
  type PhotoPersonAssertion,
  type FaceBox,
  type PhotoFacePerson,
  type PhotoFaceDecisionRequest,
} from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { recordRecentPerson } from "./people.svelte.js";
import { randomUUID } from "./crypto.js";
import type { HiddenAccess } from "./hiddenAccess.svelte.js";

export class PhotoFaces {
  faces = $state.raw<PhotoFace[]>([]);
  assertions = $state.raw<PhotoPersonAssertion[]>([]);
  people = $state.raw<PhotoFacePerson[]>([]);
  version = $state("");
  loading = $state(false);
  concealed = $state(true);
  busy = $state(false);
  error = $state("");
  pending = $state.raw<{ version: string; box: FaceBox; face_id: string }>();
  private pendingMark?: { version: string; name: string; person_id: string };
  private controller?: AbortController;
  private writer?: AbortController;
  private disposed = false;
  constructor(
    private session: string,
    readonly onauthfailure: (cause: unknown) => void,
    readonly hidden = false,
    public ondecision: (error?: string, refresh?: boolean) => void = () => {},
    readonly hiddenAccess?: HiddenAccess,
  ) {}
  private request<T>(request: () => Promise<T>) {
    return this.hiddenAccess ? this.hiddenAccess.run(request) : request();
  }
  person(face: PhotoFace) {
    return this.assertions.find(
      (row) => row.assertion_id === face.assertion_id && row.action === "assert",
    );
  }
  ordered = $derived(
    this.faces
      .filter((face) => !face.dismissed)
      .sort(
        (a, b) => a.created_at.localeCompare(b.created_at) || a.face_id.localeCompare(b.face_id),
      ),
  );
  private numbers = $derived(new Map(this.ordered.map((face, index) => [face.face_id, index + 1])));
  number(face: PhotoFace) {
    return `Face ${this.numbers.get(face.face_id) ?? 0}`;
  }
  label(face: PhotoFace) {
    const person = this.person(face);
    return person ? this.personLabel(person.person_id) : this.number(face);
  }
  caption(face: PhotoFace) {
    return this.person(face) ? `${this.number(face)} · ${this.label(face)}` : this.label(face);
  }
  personLabel(id: string) {
    return this.people.find((person) => person.person_id === id)?.display_name ?? "Unnamed person";
  }
  personActive(id: string) {
    return this.people.some((person) => person.person_id === id && person.state !== "retired");
  }
  conceal() {
    this.controller?.abort();
    this.concealed = true;
    this.loading = false;
    this.faces = [];
    this.assertions = [];
    this.people = [];
  }
  private reloading() {
    if (!this.loading && !this.concealed) return false;
    this.error = "People are reloading; try again.";
    return true;
  }
  async load(version: string) {
    if (this.disposed) return;
    this.controller?.abort();
    const changed = version !== this.version;
    if (changed) {
      this.writer?.abort();
      this.busy = false;
      this.pending = undefined;
      this.pendingMark = undefined;
    }
    const controller = (this.controller = new AbortController());
    this.version = version;
    this.concealed = !version;
    if (changed || !version) {
      this.faces = [];
      this.assertions = [];
      this.people = [];
    }
    this.error = "";
    this.loading = !!version;
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
  private async write(action: (version: string, signal: AbortSignal) => Promise<unknown>) {
    if (this.disposed || this.reloading() || this.busy || !this.version) return false;
    const version = this.version,
      controller = (this.writer = new AbortController());
    this.busy = true;
    this.error = "";
    try {
      await this.request(() => action(version, controller.signal));
      if (controller.signal.aborted) return false;
      this.ondecision();
      if (this.version === version && !this.concealed) await this.load(version);
      return true;
    } catch (cause) {
      if (!controller.signal.aborted) {
        if (this.version === version && !this.concealed) await this.load(version);
        this.failed(cause);
        this.ondecision(this.error, !(cause instanceof APIError) || cause.status >= 500);
      }
      return false;
    } finally {
      if (!controller.signal.aborted) this.busy = false;
    }
  }
  private decide(decision: PhotoFaceDecisionRequest) {
    const operation_id = randomUUID();
    return this.write(async (version, signal) => {
      const receipt = await decidePhotoFace(
        version,
        { ...decision, operation_id },
        { session: this.session, signal },
      );
      if (decision.action === "name") {
        const id = receipt.person_id || decision.person_id;
        if (id) recordRecentPerson(this.session, id);
      }
      return receipt;
    });
  }
  name(face: PhotoFace, personID: string, personName?: string) {
    return this.decide({
      action: "name",
      face_id: face.face_id,
      revision: face.revision,
      ...(personName === undefined ? { person_id: personID } : { person_name: personName }),
    });
  }
  clear(face: PhotoFace) {
    return this.decide({ action: "retract", face_id: face.face_id, revision: face.revision });
  }
  remove(face: PhotoFace) {
    return this.decide({ action: "dismiss", face_id: face.face_id, revision: face.revision });
  }
  clearMark({ assertion_id, revision }: PhotoPersonAssertion) {
    return this.decide({ action: "retract", assertion_id, revision });
  }
  async mark(personID: string, personName?: string) {
    const saved = await this.write(async (version, signal) => {
      if (personName !== undefined) {
        if (this.pendingMark?.version !== version || this.pendingMark.name !== personName)
          this.pendingMark = { version, name: personName, person_id: randomUUID() };
        personID = this.pendingMark.person_id;
      }
      const result = await createPhotoFace(
        version,
        { person_id: personID, ...(personName === undefined ? {} : { person_name: personName }) },
        { session: this.session, signal },
      );
      recordRecentPerson(this.session, result.assertion?.person_id || personID);
      return result;
    });
    if (saved) this.pendingMark = undefined;
    return saved;
  }
  async add(box: FaceBox) {
    if (this.disposed || this.reloading() || this.pending || this.busy || !this.version)
      return false;
    this.pending = { version: this.version, box: { ...box }, face_id: randomUUID() };
    return this.retryAdd();
  }
  async retryAdd() {
    const pending = this.pending;
    if (
      this.disposed ||
      this.reloading() ||
      !pending ||
      this.busy ||
      pending.version !== this.version
    )
      return false;
    const controller = (this.writer = new AbortController());
    this.busy = true;
    this.error = "";
    try {
      await this.request(() =>
        createPhotoFace(
          pending.version,
          { box: pending.box, face_id: pending.face_id },
          { session: this.session, signal: controller.signal },
        ),
      );
      if (controller.signal.aborted) return false;
      this.pending = undefined;
      if (this.disposed) return true;
      if (this.version === pending.version && !this.concealed) await this.load(pending.version);
      return !controller.signal.aborted;
    } catch (cause) {
      if (controller.signal.aborted || this.disposed) return false;
      if (cause instanceof APIError && cause.status >= 400 && cause.status < 500)
        this.pending = undefined;
      if (this.version === pending.version && !this.concealed) await this.load(pending.version);
      if (controller.signal.aborted) return false;
      if (!this.pending && this.faces.some((face) => face.face_id === pending.face_id)) return true;
      this.failed(cause);
      return false;
    } finally {
      if (!controller.signal.aborted) this.busy = false;
    }
  }
  dispose() {
    this.disposed = true;
    this.controller?.abort();
  }
}
