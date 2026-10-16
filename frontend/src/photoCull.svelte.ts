import { createSnapshot, captureSnapshotTargets } from "./snapshots.js";
import type { Query } from "./query.js";
import { randomUUID } from "./crypto.js";
import type {
  CullCheckpoint,
  CullProgress,
  CullRun,
  CullScope,
  CullStorage,
} from "./photoCullStorage.js";
import { lookupPhotoRows, photoQuery } from "./photos.svelte.js";
import { listPhotoAssets, type PhotoBrowseRowFlag } from "./generated/docbank.js";

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
  private capture?: AbortController;
  private flagRead?: AbortController;
  private flags?: Map<string, PhotoBrowseRowFlag>;
  // nextPass snapshots the checkpoint revision, so no other update may bump it until the pass is saved.
  private changingPass = false;
  // Sets of the ids in each marker list, so a new mark costs O(1) however large the query is.
  private marked = new WeakMap<string[], Set<string>>();
  private writes: Promise<void> = Promise.resolve();
  constructor(
    readonly session: string,
    private storage: CullStorage,
  ) {}
  get currentID() {
    return this.run?.checkpoint.pass[this.run.checkpoint.position];
  }
  get complete() {
    return !!this.run && !this.currentID;
  }
  async restore(quiet = false) {
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
          typeof checkpoint.passID !== "string" ||
          !checkpoint.passID.trim() ||
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
      this.flags = undefined;
      if (this.run)
        for (const kind of ["visited", "skipped", "unavailable"] as const) {
          const list = this.run.checkpoint[kind];
          this.marked.set(list, new Set(list));
        }
      this.stale = false;
      return !!this.run;
    } catch (cause) {
      if (!quiet) this.error = failure(cause);
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
  async start(query: Query, confirm = (message: string) => window.confirm(message)) {
    if (this.capturing) return false;
    const controller = (this.capture = new AbortController());
    this.capturing = true;
    this.error = "";
    try {
      await this.writes;
      const previous = await this.storage.load();
      if (previous && !confirm("Replace saved cull progress?")) return false;
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
          passID: randomUUID(),
          revision: 0,
          pass: ids.filter((id) => flags.get(id) === ""),
          position: 0,
          visited: [],
          skipped: [],
          unavailable: ids.filter((id) => !flags.has(id)),
          scope: "unflagged",
          autoAdvance: true,
        },
      };
      controller.signal.throwIfAborted();
      await this.writes;
      controller.signal.throwIfAborted();
      await this.storage.replace(run, controller.signal, previous?.checkpoint);
      this.stale = false;
      this.run = run;
      this.flags = flags;
      this.visit();
      return true;
    } catch (cause) {
      if (!controller.signal.aborted) this.error = failure(cause);
      return false;
    } finally {
      this.capturing = false;
      this.progress = "";
    }
  }
  private async refreshFlags(signal: AbortSignal) {
    const ids = this.flags ? this.run!.checkpoint.pass : this.run!.ids;
    const rows = await lookupPhotoRows(this.session, ids, signal, (loaded, total) => {
      this.progress = `Checking ${loaded} of ${total} photos…`;
    });
    signal.throwIfAborted();
    const flags = this.flags ?? new Map<string, PhotoBrowseRowFlag>();
    for (const id of ids) flags.delete(id);
    for (const row of rows) flags.set(row.asset_id, row.flag);
    this.flags = flags;
    return flags;
  }
  private async browseScope(scope: CullScope, signal: AbortSignal, firstMatch = false) {
    const captured = new Set(this.run!.ids);
    const matches = new Set<string>();
    const flag = scope === "picks" ? "pick" : scope === "rejects" ? "reject" : "";
    let cursor: string | undefined;
    do {
      const page = await listPhotoAssets(
        {
          query: {
            ...photoQuery,
            filters: scope === "all" ? {} : { flags: [flag] },
          },
          page_size: 250,
          hidden: false,
          cursor,
        },
        { session: this.session, signal },
      );
      signal.throwIfAborted();
      for (const row of page.items) {
        if (!captured.has(row.asset_id)) continue;
        matches.add(row.asset_id);
        this.flags!.set(row.asset_id, row.flag);
      }
      if (firstMatch && matches.size) return matches;
      cursor = page.next_cursor;
    } while (cursor);
    if (scope === "all") for (const id of captured) if (!matches.has(id)) this.flags!.delete(id);
    return matches;
  }

  private update(
    patch: Partial<
      Pick<CullCheckpoint, "position" | "autoAdvance" | "visited" | "skipped" | "unavailable">
    >,
  ) {
    if (!this.run || this.stale || this.changingPass) return;
    const previous = this.run.checkpoint;
    const checkpoint = { ...previous, ...patch, revision: previous.revision + 1 };
    this.run = { ...this.run, checkpoint };
    const progress: CullProgress = {
      runID: previous.runID,
      passID: previous.passID,
      revision: previous.revision,
      position: patch.position,
      autoAdvance: patch.autoAdvance,
    };
    for (const kind of ["visited", "skipped", "unavailable"] as const)
      if (patch[kind]?.length)
        progress.marker = { kind, id: patch[kind].at(-1)!, index: patch[kind].length - 1 };
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
  private mark(kind: "visited" | "skipped" | "unavailable", id: string) {
    if (!this.run || this.stale || this.changingPass) return;
    const list = this.run.checkpoint[kind];
    let ids = this.marked.get(list);
    if (!ids) this.marked.set(list, (ids = new Set(list)));
    if (ids.has(id)) return;
    ids.add(id);
    list.push(id);
    this.update({ [kind]: list });
  }
  private visit() {
    const id = this.currentID;
    if (id) this.mark("visited", id);
  }
  choose(id: string) {
    if (this.busy || this.stale) return;
    const position = this.run?.checkpoint.pass.indexOf(id) ?? -1;
    if (position >= 0) {
      this.update({ position });
      this.visit();
    }
  }
  move(direction: -1 | 1) {
    if (!this.run || this.busy || this.stale) return;
    this.update({
      position: Math.max(
        0,
        Math.min(this.run.checkpoint.pass.length, this.run.checkpoint.position + direction),
      ),
    });
    this.visit();
  }
  skip() {
    if (!this.currentID || this.busy || this.stale) return;
    this.mark("skipped", this.currentID);
    this.move(1);
  }
  unavailable(id: string) {
    this.mark("unavailable", id);
  }
  toggleAutoAdvance() {
    if (this.run) this.update({ autoAdvance: !this.run.checkpoint.autoAdvance });
  }
  private beginFlagRead() {
    this.flagRead?.abort();
    this.busy = true;
    return (this.flagRead = new AbortController());
  }
  private finishFlagRead(controller: AbortController) {
    if (this.flagRead !== controller) return;
    this.flagRead = undefined;
    this.busy = false;
    this.progress = "";
  }
  async checkRemainder() {
    if (!this.run || !this.complete || this.stale) return;
    const controller = this.beginFlagRead();
    this.remainder = false;
    try {
      await this.refreshFlags(controller.signal);
      const matches = await this.browseScope("unflagged", controller.signal, true);
      controller.signal.throwIfAborted();
      this.remainder = matches.size > 0;
    } catch (cause) {
      if (!controller.signal.aborted) this.error = failure(cause);
    } finally {
      this.finishFlagRead(controller);
    }
  }
  async nextPass(followUp = false, confirm = (message: string) => window.confirm(message)) {
    if (!this.run || this.busy || this.stale) return;
    if (!followUp && !this.complete && !confirm("Change scope and reset this pass's progress?"))
      return;
    const controller = this.beginFlagRead();
    this.changingPass = true;
    let changed = false;
    this.error = "";
    try {
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
      const matches = await this.browseScope(scope, controller.signal);
      controller.signal.throwIfAborted();
      const pass = ids.filter((id) => matches.has(id));
      if (followUp && !pass.length) {
        this.remainder = false;
        return;
      }
      const checkpoint: CullCheckpoint = {
        ...previous,
        passID: randomUUID(),
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
          passID: previous.passID,
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
      if (!controller.signal.aborted) this.error = failure(cause);
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
