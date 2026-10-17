import { createSnapshot, captureSnapshotTargets } from "./snapshots.js";
import type { Query } from "./query.js";
import { randomUUID } from "./crypto.js";
import { APIError } from "./api-transport.js";
import { CullFormatError, sameCheckpoint } from "./photoCullStorage.js";
import type {
  CullCheckpoint,
  CullProgress,
  CullRun,
  CullScope,
  CullStorage,
} from "./photoCullStorage.js";
import { lookupPhotoFlags } from "./generated/docbank.js";
import type { PhotoBrowseRowFlag } from "./generated/docbank.js";

// Keep in sync with lookupPhotoFlags asset_ids maxItems.
const flagLookupLimit = 20_000;
const scopes: CullScope[] = ["unflagged", "picks", "rejects", "all"];
const failure = (cause: unknown) => (cause instanceof Error ? cause.message : String(cause));

export class PhotoCull {
  run = $state.raw<CullRun>();
  busy = $state(false);
  error = $state("");
  capturing = $state(false);
  stale = $state(false);
  progress = $state("");
  remainder = $state(false);
  checkingRemainder = $state(false);
  // The pass whose remainder was last read; flag edits clear it so the next completion reads again.
  private remainderPass?: string[];
  private capture?: AbortController;
  private flagRead?: AbortController;
  saved = $state(false);
  async probe() {
    try {
      this.saved = !!(await this.storage.saved());
    } catch (cause) {
      this.saved = false;
      if (cause instanceof CullFormatError) this.error = failure(cause);
    }
  }
  // nextPass snapshots the checkpoint revision, so no other update may bump it until the pass is saved.
  private changingPass = false;
  // Sets of the ids in each marker list, so a new mark costs O(1) however large the query is.
  private marked = new WeakMap<string[], Set<string>>();
  private writes: Promise<void> = Promise.resolve();
  constructor(
    readonly session: string,
    private storage: CullStorage,
    private onauthfailure?: (cause: unknown) => void,
  ) {}
  private apiFailure(cause: unknown) {
    if (cause instanceof APIError && cause.status === 401) this.onauthfailure?.(cause);
    this.error = failure(cause);
  }
  get currentID() {
    return this.run?.checkpoint.pass[this.run.checkpoint.position];
  }
  // Runs in the write chain, so the saved revision matches every update queued before this check.
  async verifyDecision() {
    if (!this.run || this.stale || this.busy) return false;
    const expected = this.run.checkpoint;
    let verified = false;
    const check = this.writes.then(async () => {
      if (this.stale) return;
      try {
        const saved = await this.storage.saved();
        if (!saved || !sameCheckpoint(saved, expected))
          throw new Error("Cull progress changed in another tab. Close and resume it.");
        verified = true;
      } catch (cause) {
        this.stale = true;
        this.error = `Progress could not be saved: ${failure(cause)}`;
      }
    });
    this.writes = check;
    await check;
    return verified && !this.stale && !this.busy;
  }
  get complete() {
    return !!this.run && !this.currentID;
  }
  async restore() {
    await this.writes;
    this.error = "";
    try {
      const run = await this.storage.load();
      if (run !== undefined) {
        const checkpoint = run?.checkpoint;
        if (
          !run ||
          !checkpoint ||
          typeof checkpoint.runID !== "string" ||
          !checkpoint.runID.trim() ||
          !Number.isSafeInteger(checkpoint.revision) ||
          checkpoint.revision < 0 ||
          typeof checkpoint.autoAdvance !== "boolean" ||
          ![
            run.ids,
            checkpoint.pass,
            checkpoint.visited,
            checkpoint.skipped,
            checkpoint.unavailable,
          ].every((ids) => Array.isArray(ids) && ids.every((id) => typeof id === "string")) ||
          !scopes.includes(checkpoint.scope) ||
          !Number.isInteger(checkpoint.position) ||
          checkpoint.position < 0 ||
          checkpoint.position > checkpoint.pass.length
        )
          throw new Error("Saved cull progress is invalid. Start a new cull.");
        const captured = new Set(run.ids);
        if (!checkpoint.pass.every((id) => captured.has(id)))
          throw new Error("Saved cull progress is invalid. Start a new cull.");
      }
      this.run = run;
      this.saved = !!run;
      if (this.run)
        for (const kind of ["visited", "skipped", "unavailable"] as const) {
          const list = this.run.checkpoint[kind];
          this.marked.set(list, new Set(list));
        }
      this.stale = false;
      return !!this.run;
    } catch (cause) {
      this.error = failure(cause);
      return false;
    }
  }
  cancel() {
    this.capture?.abort();
    if (this.flagRead) {
      this.flagRead.abort();
      this.finishFlagRead(this.flagRead);
    }
  }
  async start(
    query: Query,
    confirm: (message: string) => boolean | Promise<boolean> = () => false,
  ) {
    if (this.capturing) return false;
    const controller = (this.capture = new AbortController());
    this.capturing = true;
    this.error = "";
    try {
      await this.writes;
      const previous = await this.storage.saved().catch((cause) => {
        if (cause instanceof CullFormatError) {
          this.error = cause.message;
          return cause.saved;
        }
        throw cause;
      });
      if (previous && !(await confirm("Replace saved cull progress?"))) return false;
      this.progress = "Capturing photos…";
      const page = await createSnapshot(
        this.session,
        query,
        { photos: true, page_size: 250 },
        controller.signal,
      );
      const captured = await captureSnapshotTargets(
        this.session,
        page,
        controller.signal,
        {},
        (loaded, total) => {
          this.progress = `Capturing ${loaded} of ${total} photos…`;
        },
      );
      const ids = captured.members.map((member) => {
        if (!member.asset_id) throw new Error("Photo snapshot membership was incomplete.");
        return member.asset_id;
      });
      const flags = new Map(captured.members.map((member) => [member.asset_id!, member.flag!]));
      const run: CullRun = {
        ids,
        checkpoint: {
          runID: randomUUID(),
          revision: 0,
          pass: ids.filter((id) => flags.get(id) === ""),
          position: 0,
          visited: [],
          skipped: [],
          unavailable: [],
          scope: "unflagged",
          autoAdvance: true,
        },
      };
      controller.signal.throwIfAborted();
      await this.writes;
      controller.signal.throwIfAborted();
      await this.storage.replace(run, controller.signal, previous);
      this.error = "";
      this.stale = false;
      this.run = run;
      this.saved = true;
      this.visit();
      return true;
    } catch (cause) {
      if (!controller.signal.aborted) this.apiFailure(cause);
      return false;
    } finally {
      this.capturing = false;
      this.progress = "";
    }
  }
  private async refreshFlags(signal: AbortSignal) {
    const ids = this.run!.ids;
    const flags = new Map<string, PhotoBrowseRowFlag>();
    for (let offset = 0; offset < ids.length; offset += flagLookupLimit) {
      signal.throwIfAborted();
      const batch = ids.slice(offset, offset + flagLookupLimit);
      this.progress = `Checking ${offset} of ${ids.length} photos…`;
      const { items } = await lookupPhotoFlags(
        { asset_ids: batch },
        { session: this.session, signal },
      );
      signal.throwIfAborted();
      for (const row of items) flags.set(row.asset_id, row.flag);
    }
    return flags;
  }
  private matches(scope: CullScope, flags: Map<string, PhotoBrowseRowFlag>) {
    const flag = scope === "picks" ? "pick" : scope === "rejects" ? "reject" : "";
    return this.run!.ids.filter(
      (id) => flags.has(id) && (scope === "all" || flags.get(id) === flag),
    );
  }

  private update(
    patch: Partial<
      Pick<CullCheckpoint, "position" | "autoAdvance" | "visited" | "skipped" | "unavailable">
    >,
    marker?: CullProgress["marker"],
  ) {
    if (!this.run || this.stale || this.changingPass) return;
    const previous = this.run.checkpoint;
    const checkpoint = { ...previous, ...patch, revision: previous.revision + 1 };
    this.run = { ...this.run, checkpoint };
    const progress: CullProgress = {
      runID: previous.runID,
      revision: previous.revision,
      position: patch.position,
      autoAdvance: patch.autoAdvance,
      marker,
    };
    this.writes = this.writes
      .then(async () => {
        if (!this.stale) await this.storage.checkpoint(progress);
      })
      .catch((cause) => {
        this.stale = true;
        this.error = `Progress could not be saved: ${failure(cause)}`;
      });
  }
  // Appends in place: copying a 250,000-photo list on every photo would make culling quadratic.
  private mark(kind: "visited" | "skipped" | "unavailable", added: string[]) {
    if (!this.run || this.stale || this.changingPass) return;
    const list = this.run.checkpoint[kind];
    let ids = this.marked.get(list);
    if (!ids) this.marked.set(list, (ids = new Set(list)));
    const index = list.length;
    for (const id of added) {
      if (ids.has(id)) continue;
      ids.add(id);
      list.push(id);
    }
    if (list.length > index) this.update({ [kind]: list }, { kind, ids: list.slice(index), index });
  }
  private visit() {
    const id = this.currentID;
    if (id) this.mark("visited", [id]);
  }
  choose(id: string) {
    if (this.busy || this.stale) return;
    const position = this.run?.checkpoint.pass.indexOf(id) ?? -1;
    if (position >= 0 && position !== this.run?.checkpoint.position) {
      this.update({ position });
      this.visit();
    }
  }
  move(direction: -1 | 1) {
    if (!this.run || (this.busy && !this.checkingRemainder) || this.stale) return;
    const position = Math.max(
      0,
      Math.min(this.run.checkpoint.pass.length, this.run.checkpoint.position + direction),
    );
    if (position === this.run.checkpoint.position) return;
    // Leaving the completion screen abandons its remainder read.
    if (this.flagRead) {
      this.flagRead.abort();
      this.finishFlagRead(this.flagRead);
    }
    this.update({ position });
    this.visit();
  }
  skip() {
    if (!this.currentID || this.busy || this.stale) return;
    this.mark("skipped", [this.currentID]);
    this.move(1);
  }
  unavailable(id: string) {
    this.mark("unavailable", [id]);
  }
  toggleAutoAdvance() {
    if (this.run) this.update({ autoAdvance: !this.run.checkpoint.autoAdvance });
  }
  private beginFlagRead(remainder = false) {
    this.flagRead?.abort();
    this.busy = true;
    this.checkingRemainder = remainder;
    return (this.flagRead = new AbortController());
  }
  private finishFlagRead(controller: AbortController) {
    if (this.flagRead !== controller) return;
    this.flagRead = undefined;
    this.busy = false;
    this.checkingRemainder = false;
    this.progress = "";
  }
  flagsChanged() {
    this.remainderPass = undefined;
  }
  async checkRemainder() {
    if (!this.run || !this.complete || this.stale) return;
    const pass = this.run.checkpoint.pass;
    if (this.remainderPass === pass) return;
    const controller = this.beginFlagRead(true);
    this.error = "";
    this.remainder = false;
    this.remainderPass = undefined;
    try {
      const flags = await this.refreshFlags(controller.signal);
      controller.signal.throwIfAborted();
      this.mark(
        "unavailable",
        this.run.ids.filter((id) => !flags.has(id)),
      );
      this.remainder = this.matches("unflagged", flags).length > 0;
      this.remainderPass = pass;
    } catch (cause) {
      if (!controller.signal.aborted) this.apiFailure(cause);
    } finally {
      this.finishFlagRead(controller);
    }
  }
  async nextPass(
    followUp = false,
    confirm: (message: string) => boolean | Promise<boolean> = () => false,
  ) {
    if (!this.run || this.busy || this.stale) return;
    const controller = this.beginFlagRead();
    this.changingPass = true;
    let changed = false;
    this.error = "";
    try {
      if (
        !followUp &&
        !this.complete &&
        !(await confirm("Change scope and reset this pass's progress?"))
      )
        return;
      await this.writes;
      controller.signal.throwIfAborted();
      if (this.stale) return;
      const previous = this.run.checkpoint;
      const ids = this.run.ids;
      const flags = await this.refreshFlags(controller.signal);
      controller.signal.throwIfAborted();
      const scope = followUp
        ? "unflagged"
        : scopes[(scopes.indexOf(this.run.checkpoint.scope) + 1) % scopes.length]!;
      const pass = this.matches(scope, flags);
      if (followUp && !pass.length) {
        this.changingPass = false;
        this.mark(
          "unavailable",
          ids.filter((id) => !flags.has(id)),
        );
        this.remainder = false;
        return;
      }
      const checkpoint: CullCheckpoint = {
        ...previous,
        revision: previous.revision + 1,
        scope,
        pass,
        position: 0,
        visited: [],
        skipped: [],
        unavailable: ids.filter((id) => !flags.has(id)),
      };
      try {
        await this.storage.checkpoint({
          runID: previous.runID,
          revision: previous.revision,
          pass: checkpoint,
        });
      } catch (cause) {
        this.stale = true;
        throw cause;
      }
      this.run = { ...this.run, checkpoint };
      this.remainder = false;
      changed = true;
    } catch (cause) {
      if (!controller.signal.aborted) this.apiFailure(cause);
    } finally {
      this.changingPass = false;
      this.finishFlagRead(controller);
    }
    if (changed) this.visit();
  }
  flush() {
    return this.writes;
  }
}
