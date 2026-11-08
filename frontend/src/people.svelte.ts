import {
  decideFaceCandidate,
  decidePhotoFace,
  listFaceCandidates,
  listPeople,
  renamePerson,
  mergePerson,
  type FaceCandidate,
  type FaceCandidateDecisionRequest,
  type FaceSuggestionFace,
  type Person,
  type PersonMergeReceipt,
  type PersonSummary,
} from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import { randomUUID } from "./crypto.js";
import { settleReads } from "./photoEvents.js";
import { uuidV4Pattern as uuid } from "./query.js";
import type { HiddenAccess } from "./hiddenAccess.svelte.js";

async function readPerson(response: Response, expectedID: string): Promise<Person> {
  const person = await response.json();
  if (
    !person ||
    person.person_id !== expectedID ||
    typeof person.display_name !== "string" ||
    !Number.isSafeInteger(person.revision) ||
    person.revision < 1 ||
    response.headers.get("ETag") !== `"${person.revision}"`
  )
    throw new Error("Invalid person response.");
  return person;
}

async function readReceipt(
  response: Response,
  source: PersonSummary,
  survivor: PersonSummary,
  operationID: string,
): Promise<PersonMergeReceipt> {
  const receipt = await response.json();
  if (
    !receipt ||
    typeof receipt.merge_id !== "string" ||
    !uuid.test(receipt.merge_id) ||
    receipt.operation_id !== operationID ||
    receipt.absorbed_person_id !== source.person_id ||
    receipt.survivor_person_id !== survivor.person_id ||
    response.headers.get("ETag") !== `"${receipt.survivor_revision_after}"`
  )
    throw new Error("Invalid person merge response.");
  return receipt;
}

export const peoplePageSize = 24;
export type PeopleMerge = {
  pair: readonly [PersonSummary, PersonSummary];
  keep?: string;
  confirmed: boolean;
};
export const mergeRoles = ({ pair, keep }: PeopleMerge) => {
  const survivor = pair.find((person) => person.person_id === keep);
  const source = pair.find((person) => person.person_id !== keep);
  return survivor && source ? { source, survivor } : undefined;
};

export class People {
  items = $state.raw<PersonSummary[]>([]);
  // draft is the search box text; query is the search the loaded cards answer.
  draft = $state("");
  query = $state("");
  cursor = $state<string>();
  loading = $state(false);
  error = $state("");
  busy = $state(false);
  writeError = $state("");
  merge = $state.raw<PeopleMerge>();
  // selected holds up to two people to merge and survives new searches.
  selected = $state.raw<PersonSummary[]>([]);
  private controller?: AbortController;
  // epoch changes whenever the cards are concealed, as when Hidden locks, so a
  // response from an earlier scope cannot restore cards or selections.
  private epoch = 0;
  private writer?: AbortController;
  private disposed = false;
  private cursorExpired = false;
  readonly suggestions: Suggestions = new Suggestions(this);
  constructor(
    readonly session: string,
    readonly onauthfailure: (cause: unknown) => void,
    readonly access?: HiddenAccess,
    readonly onchanged: (id: string, name: string, survivorID?: string) => void = () => {},
  ) {}
  run<T>(request: () => Promise<T>) {
    return this.access ? this.access.run(request) : request();
  }
  failure(cause: unknown) {
    if (cause instanceof APIError && cause.status === 401) this.onauthfailure(cause);
    return cause instanceof Error ? cause.message : String(cause);
  }
  conceal() {
    this.controller?.abort();
    this.epoch++;
    this.selected = [];
    this.items = [];
    this.cursor = undefined;
    this.loading = false;
    this.error = "";
    this.merge = undefined;
    this.writeError = "";
    this.suggestions.conceal();
  }
  async load(query = this.query, more = false) {
    if (this.disposed) return;
    if (this.access?.lockError) {
      this.conceal();
      return;
    }
    this.controller?.abort();
    const request = (this.controller = new AbortController());
    const cursor = more ? this.cursor : undefined;
    this.query = query;
    if (!more) {
      this.items = [];
      this.cursor = undefined;
    }
    this.loading = true;
    this.error = "";
    this.cursorExpired = false;
    if (!more && !this.suggestions.started) void this.suggestions.load();
    try {
      const page = await this.run(() => {
        request.signal.throwIfAborted();
        return listPeople(
          { query, limit: peoplePageSize, photo_summary: true, ...(cursor ? { cursor } : {}) },
          { session: this.session, signal: request.signal },
        );
      });
      if (!request.signal.aborted) {
        this.items = [
          ...new Map(
            [...this.items, ...page.items].map((person) => [person.person_id, person]),
          ).values(),
        ];
        this.cursor = page.next_cursor;
      }
    } catch (cause) {
      if (!request.signal.aborted) {
        this.error = this.failure(cause);
        this.cursorExpired = cause instanceof APIError && cause.code === "cursor_expired";
      }
    } finally {
      if (!request.signal.aborted) this.loading = false;
    }
  }
  retry() {
    return this.load(this.query, !!this.items.length && !this.cursorExpired);
  }
  // refresh rereads the loaded cards in place after a change elsewhere, such
  // as another tab's rename or merge. It keeps the search, at least as many
  // cards as were loaded, and the selection, with fresh names and revisions.
  // A failure keeps the current cards and throws so the photo event pass
  // retries. The pass waits for suggestions and cards to finish together.
  async refresh() {
    await settleReads([
      this.suggestions.started ? this.suggestions.load() : undefined,
      this.refreshCards(),
    ]);
  }
  async refreshCards() {
    if (this.disposed || this.loading || this.access?.lockError) return;
    const request = (this.controller = new AbortController());
    const { query } = this;
    const wanted = Math.max(this.items.length, peoplePageSize);
    const rows: PersonSummary[] = [];
    let cursor: string | undefined;
    try {
      do {
        const page = await this.run(() => {
          request.signal.throwIfAborted();
          return listPeople(
            { query, limit: peoplePageSize, photo_summary: true, ...(cursor ? { cursor } : {}) },
            { session: this.session, signal: request.signal },
          );
        });
        rows.push(...page.items);
        cursor = page.next_cursor;
      } while (cursor && rows.length < wanted);
    } catch (cause) {
      if (request.signal.aborted) return;
      this.failure(cause);
      throw cause;
    }
    if (request.signal.aborted) return;
    const fresh = new Map(rows.map((person) => [person.person_id, person]));
    this.items = [...fresh.values()];
    this.cursor = cursor;
    this.error = "";
    this.cursorExpired = false;
    this.selected = this.selected.map((person) => fresh.get(person.person_id) ?? person);
  }
  private begin() {
    if (this.busy || this.disposed) return;
    const request = (this.writer = new AbortController());
    this.busy = true;
    this.writeError = "";
    return request;
  }
  // Uncertain writes reload the cards and require fresh selections. Rejected
  // input keeps the cards; stale or missing people also clear the selection.
  private async recover(cause: unknown, epoch: number, person?: PersonSummary, name?: string) {
    const message = this.failure(cause);
    const status = cause instanceof APIError ? cause.status : 0;
    if (epoch === this.epoch) {
      this.writeError = message;
      if (
        status === 404 ||
        status === 412 ||
        (cause instanceof APIError && cause.code === "person_retired")
      )
        this.selected = [];
      if (status !== 422) await this.load();
    }
    if (status === 0 || status >= 500) {
      if (epoch === this.epoch) this.selected = [];
      if (!person || name === undefined) return;
      try {
        let saved: PersonSummary | undefined;
        let cursor: string | undefined;
        do {
          if (this.writer?.signal.aborted) return;
          const page = await this.run(() =>
            listPeople(
              {
                query: name,
                limit: peoplePageSize,
                photo_summary: true,
                ...(cursor ? { cursor } : {}),
              },
              { session: this.session, signal: this.writer?.signal },
            ),
          );
          if (this.writer?.signal.aborted) return;
          saved ??= page.items.find(
            (row) =>
              row.person_id === person.person_id &&
              row.display_name === name &&
              row.revision > person.revision,
          );
          cursor = page.next_cursor;
        } while (cursor);
        if (saved) this.onchanged(person.person_id, saved.display_name);
      } catch (recoveryCause) {
        this.failure(recoveryCause);
      }
    }
  }
  async rename(person: PersonSummary, name: string) {
    const request = this.begin();
    if (!request) return false;
    const epoch = this.epoch;
    try {
      const response = await this.run(() =>
        renamePerson(
          person.person_id,
          { display_name: name.trim() },
          { "If-Match": `"${person.revision}"` },
          { session: this.session, signal: request.signal },
        ),
      );
      const saved = await readPerson(response, person.person_id);
      if (request.signal.aborted) return false;
      this.onchanged(person.person_id, saved.display_name);
      if (epoch === this.epoch) {
        const replace = (rows: PersonSummary[]) =>
          rows.map((row) =>
            row.person_id === person.person_id
              ? { ...row, display_name: saved.display_name, revision: saved.revision }
              : row,
          );
        this.selected = replace(this.selected);
        await this.load();
      }
      return true;
    } catch (cause) {
      if (!request.signal.aborted) {
        await this.recover(cause, epoch, person, name.trim());
      }
      return false;
    } finally {
      if (!request.signal.aborted) this.busy = false;
    }
  }
  toggle(person: PersonSummary) {
    if (this.merge) return;
    this.writeError = "";
    if (this.selected.some((row) => row.person_id === person.person_id))
      this.selected = this.selected.filter((row) => row.person_id !== person.person_id);
    else if (this.selected.length < 2) this.selected = [...this.selected, person];
  }
  clearSelection() {
    if (this.merge) return;
    this.selected = [];
    this.writeError = "";
  }
  startMerge() {
    const [first, second] = this.selected;
    if (this.busy || !first || !second) return;
    this.writeError = "";
    this.merge = { pair: [first, second], confirmed: false };
  }
  closeMerge() {
    if (this.busy) return;
    this.writeError = "";
    this.merge = undefined;
  }
  keep(id: string) {
    if (!this.merge || this.busy) return;
    this.merge = { ...this.merge, keep: id, confirmed: false };
  }
  confirmMerge(confirmed: boolean) {
    if (this.merge && !this.busy) this.merge = { ...this.merge, confirmed };
  }
  async submitMerge() {
    const selected = this.merge;
    const roles = selected && mergeRoles(selected);
    if (!selected || !roles || !selected.confirmed) return false;
    const request = this.begin();
    if (!request) return false;
    const epoch = this.epoch;
    const { source, survivor } = roles;
    const operationID = randomUUID();
    const send = async () => {
      const response = await this.run(() =>
        mergePerson(
          survivor.person_id,
          {
            absorbed_person_id: source.person_id,
            absorbed_revision: source.revision,
            operation_id: operationID,
          },
          { "If-Match": `"${survivor.revision}"` },
          { session: this.session, signal: request.signal },
        ),
      );
      await readReceipt(response, source, survivor, operationID);
    };
    try {
      await send().catch((cause: unknown) => {
        const status = cause instanceof APIError ? cause.status : 0;
        if (request.signal.aborted || (status !== 0 && status < 500)) throw cause;
        return send();
      });
      if (request.signal.aborted) return false;
      this.onchanged(source.person_id, survivor.display_name, survivor.person_id);
      if (epoch === this.epoch) {
        this.merge = undefined;
        this.selected = [];
      }
      if (epoch === this.epoch || this.items.length > 0 || this.loading) await this.load();
      return true;
    } catch (cause) {
      if (!request.signal.aborted) {
        if (epoch === this.epoch) this.merge = undefined;
        await this.recover(cause, epoch);
      }
      return false;
    } finally {
      if (!request.signal.aborted) this.busy = false;
    }
  }
  dispose() {
    this.disposed = true;
    this.conceal();
    this.writer?.abort();
    this.suggestions.dispose();
  }
}

export const suggestionTitle = (candidate: FaceCandidate) =>
  candidate.suggested_person_id ? `Is this ${candidate.display_name}?` : "Who is this?";
const plural = (count: number, noun: string) => `${count} ${noun}${count === 1 ? "" : "s"}`;
export const suggestionCounts = (faces: readonly FaceSuggestionFace[]) =>
  `${plural(faces.length, "face")} · ${plural(new Set(faces.map((face) => face.preview.asset_id)).size, "photo")}`;

// A review follows one group through face removals. removed holds the faces
// taken out in this sheet, so a reread that still lists one cannot restore it;
// a review without a candidate has nothing left.
export type SuggestionReview = {
  candidate?: FaceCandidate;
  title: string;
  removed: readonly string[];
};
export type NameSearch = {
  query: string;
  people: PersonSummary[];
  nextCursor?: string;
  pendingCursor?: string;
  incompleteName?: string;
  status: "idle" | "loading" | "ready" | "error" | "invalid";
  error: string;
};
const idleNames = (): NameSearch => ({ query: "", people: [], status: "idle", error: "" });

export async function readNamePage(
  state: NameSearch,
  options: {
    session: string;
    face?: string;
    signal: AbortSignal;
    run: <T>(request: () => Promise<T>) => Promise<T>;
    failure: (cause: unknown) => string;
  },
): Promise<NameSearch> {
  if (new TextEncoder().encode(state.query.trim()).length > 200)
    return { ...state, status: "invalid", error: "Name must be 200 bytes or fewer." };
  try {
    const page = await options.run(() => {
      options.signal.throwIfAborted();
      return listPeople(
        {
          query: state.query.trim(),
          limit: 100,
          recent_first: true,
          ...(options.face && options.face !== "mark" ? { face_id: options.face } : {}),
          ...(state.pendingCursor ? { cursor: state.pendingCursor } : {}),
        },
        { session: options.session, signal: options.signal },
      );
    });
    return {
      ...state,
      // Similarity changes can repeat a person across pages.
      people: [
        ...new Map(
          [...state.people, ...page.items].map((person) => [person.person_id, person]),
        ).values(),
      ],
      nextCursor: page.next_cursor,
      incompleteName:
        page.next_cursor && page.same_name_continues ? page.items.at(-1)!.display_name_folded : "",
      status: "ready",
      error: "",
    };
  } catch (cause) {
    return {
      ...state,
      ...(cause instanceof APIError && cause.code === "cursor_expired"
        ? { people: [], pendingCursor: undefined, nextCursor: undefined, incompleteName: "" }
        : {}),
      status: "error",
      error: options.failure(cause),
    };
  }
}
// reviewFaces lists the faces a review still shows, in the server's timeline order.
export const reviewFaces = ({ candidate, removed }: SuggestionReview) =>
  (candidate?.faces ?? []).filter((face) => !removed.includes(face.face_id));

// Suggestions holds the People view's face candidates and the review sheet.
// It shares the cards' session, Hidden access, and concealment.
export class Suggestions {
  items = $state.raw<FaceCandidate[]>([]);
  total = $state(0);
  loading = $state(false);
  error = $state("");
  busy = $state(false);
  writeError = $state("");
  review = $state.raw<SuggestionReview>();
  names = $state.raw<NameSearch>(idleNames());
  started = false;
  private controller?: AbortController;
  private writer?: AbortController;
  private search?: AbortController;
  private epoch = 0;
  private disposed = false;
  constructor(readonly people: People) {}
  conceal() {
    this.writer?.abort();
    this.busy = false;
    this.controller?.abort();
    this.search?.abort();
    this.epoch++;
    this.started = false;
    this.items = [];
    this.total = 0;
    this.loading = false;
    this.error = "";
    this.review = undefined;
    this.names = idleNames();
    this.writeError = "";
  }
  private read(offset: number, signal: AbortSignal) {
    return this.people.run(() => {
      signal.throwIfAborted();
      return listFaceCandidates(
        { limit: peoplePageSize, offset },
        {
          session: this.people.session,
          signal: AbortSignal.any([signal, AbortSignal.timeout(30_000)]),
        },
      );
    });
  }
  private current(request: AbortController, epoch: number) {
    return (
      !this.disposed &&
      epoch === this.epoch &&
      this.controller === request &&
      !request.signal.aborted
    );
  }
  async load(more = false) {
    if (this.disposed || this.people.access?.lockError) return;
    this.started = true;
    this.controller?.abort();
    const request = (this.controller = new AbortController());
    const epoch = this.epoch;
    const wanted = more
      ? this.items.length + peoplePageSize
      : Math.max(this.items.length, peoplePageSize);
    this.loading = true;
    this.error = "";
    let rows: FaceCandidate[] = more ? [...this.items] : [];
    let offset = rows.length;
    try {
      let total = 0;
      do {
        const page = await this.read(offset, request.signal);
        if (!this.current(request, epoch)) return;
        offset += page.items.length;
        rows = [
          ...new Map([...rows, ...page.items].map((item) => [item.candidate_id, item])).values(),
        ];
        total = page.total;
        if (!page.items.length) break;
      } while (offset < Math.min(wanted, total));
      this.items = rows;
      this.total = total;
    } catch (cause) {
      if (this.current(request, epoch)) {
        this.error = this.people.failure(cause);
      }
    } finally {
      if (this.current(request, epoch)) this.loading = false;
    }
  }
  retry() {
    return this.load();
  }
  open(candidate: FaceCandidate) {
    if (this.busy || this.disposed) return;
    this.search?.abort();
    this.writeError = "";
    this.names = idleNames();
    this.review = { candidate, title: suggestionTitle(candidate), removed: [] };
  }
  close() {
    if (this.busy || this.disposed) return;
    this.search?.abort();
    this.writeError = "";
    this.names = idleNames();
    this.review = undefined;
  }
  // searchNames lists people for the "Who is this?" name picker, closest to
  // the group's first face when it has a vector.
  async searchNames(query: string, more = false) {
    if (this.disposed || !this.review) return;
    this.search?.abort();
    const request = (this.search = new AbortController());
    const epoch = this.epoch;
    const current = () =>
      !this.disposed && epoch === this.epoch && this.search === request && !request.signal.aborted;
    const face = this.review && reviewFaces(this.review)[0];
    const previous = more ? this.names : idleNames();
    const cursor = more
      ? previous.status === "error"
        ? previous.pendingCursor
        : previous.nextCursor
      : undefined;
    this.names = { ...previous, query, pendingCursor: cursor, status: "loading", error: "" };
    const next = await readNamePage(this.names, {
      session: this.people.session,
      face: face?.face_id,
      signal: request.signal,
      run: (read) => this.people.run(read),
      failure: (cause) => (current() ? this.people.failure(cause) : ""),
    });
    if (current()) this.names = next;
  }
  private async write(send: (signal: AbortSignal) => Promise<unknown>, settled: () => void) {
    if (this.busy || this.disposed || !this.review?.candidate) return false;
    const request = (this.writer = new AbortController());
    const epoch = this.epoch;
    this.busy = true;
    this.writeError = "";
    try {
      await send(request.signal);
      if (request.signal.aborted || epoch !== this.epoch) return false;
      settled();
      await this.load();
      return !request.signal.aborted && epoch === this.epoch;
    } catch (cause) {
      if (request.signal.aborted || epoch !== this.epoch) return false;
      this.writeError = this.people.failure(cause);
      if (cause instanceof APIError && [404, 409, 412].includes(cause.status)) {
        this.search?.abort();
        this.review = undefined;
        this.names = idleNames();
        await this.load();
      }
      return false;
    } finally {
      if (!request.signal.aborted && epoch === this.epoch && !this.disposed) this.busy = false;
    }
  }
  // Reject a proposed name durably. An unnamed crop is omitted from this review.
  removeFace(face: FaceSuggestionFace) {
    const review = this.review;
    const person = review?.candidate?.suggested_person_id;
    if (review && !person) {
      const next = { ...review, removed: [...review.removed, face.face_id] };
      this.review = reviewFaces(next).length ? next : { ...next, candidate: undefined };
      return Promise.resolve(true);
    }
    return this.write(
      (signal) =>
        this.people.run(() =>
          decidePhotoFace(
            face.content_version_id,
            {
              action: "reject",
              face_id: face.face_id,
              revision: face.revision,
              operation_id: randomUUID(),
              ...(person ? { person_id: person } : {}),
            },
            { session: this.people.session, signal },
          ),
        ),
      () => {
        this.search?.abort();
        this.review = undefined;
        this.names = idleNames();
      },
    );
  }
  // decide settles the whole group and closes the sheet. A confirmation
  // changes people's photos, so the cards refresh too.
  async decide(decision: FaceCandidateDecisionRequest) {
    const candidate = this.review?.candidate;
    if (!candidate) return false;
    if (decision.action !== "reject")
      decision = { ...decision, face_ids: reviewFaces(this.review!).map((face) => face.face_id) };
    const saved = await this.write(
      (signal) =>
        this.people.run(() =>
          decideFaceCandidate(
            candidate.candidate_id,
            decision,
            { "If-Match": `"${candidate.revision}"` },
            { session: this.people.session, signal },
          ),
        ),
      () => {
        this.search?.abort();
        this.names = idleNames();
        this.review = undefined;
      },
    );
    if (saved && decision.action !== "reject")
      await this.people.refreshCards().catch(() => undefined);
    return saved;
  }
  dispose() {
    this.disposed = true;
    this.conceal();
    this.writer?.abort();
  }
}
