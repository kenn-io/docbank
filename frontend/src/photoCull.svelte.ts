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
import { lookupPhotoRows } from "./photos.svelte.js";

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
      this.run = await this.storage.load();
      this.stale = false;
      return !!this.run;
    } catch (cause) {
      if (!quiet) this.error = failure(cause);
      return false;
    }
  }
  cancel() {
    this.capture?.abort();
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
  private async refreshFlags(ids: string[], signal?: AbortSignal) {
    const rows = await lookupPhotoRows(this.session, ids, signal, (loaded, total) => {
      this.progress = `Checking ${loaded} of ${total} photos…`;
    });
    return new Map(rows.map((row) => [row.asset_id, row.flag]));
  }

  private update(
    patch: Partial<
      Pick<CullCheckpoint, "position" | "autoAdvance" | "visited" | "skipped" | "unavailable">
    >,
  ) {
    if (!this.run || this.stale) return;
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
  private visit() {
    const id = this.currentID;
    if (id && !this.run!.checkpoint.visited.includes(id))
      this.update({ visited: [...this.run!.checkpoint.visited, id] });
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
    this.update({ skipped: [...new Set([...this.run!.checkpoint.skipped, this.currentID])] });
    this.move(1);
  }
  unavailable(id: string) {
    if (this.run && !this.run.checkpoint.unavailable.includes(id))
      this.update({ unavailable: [...this.run.checkpoint.unavailable, id] });
  }
  toggleAutoAdvance() {
    if (this.run) this.update({ autoAdvance: !this.run.checkpoint.autoAdvance });
  }
  async checkRemainder() {
    if (!this.run || !this.complete || this.busy || this.stale) return;
    this.busy = true;
    this.remainder = false;
    try {
      const flags = await this.refreshFlags(this.run.ids);
      this.remainder = [...flags.values()].includes("");
    } catch (cause) {
      this.error = failure(cause);
    } finally {
      this.busy = false;
      this.progress = "";
    }
  }
  async nextPass(followUp = false, confirm = (message: string) => window.confirm(message)) {
    if (!this.run || this.busy || this.stale) return;
    if (!followUp && !this.complete && !confirm("Change scope and reset this pass's progress?"))
      return;
    this.busy = true;
    this.error = "";
    try {
      await this.writes;
      if (this.stale) return;
      const previous = this.run.checkpoint;
      const ids = this.run.ids;
      const flags = await this.refreshFlags(ids);
      const scope = followUp
        ? "unflagged"
        : scopes[(scopes.indexOf(this.run.checkpoint.scope) + 1) % scopes.length]!;
      const pass = ids.filter(
        (id) =>
          flags.has(id) &&
          (scope === "all" ||
            flags.get(id) === (scope === "picks" ? "pick" : scope === "rejects" ? "reject" : "")),
      );
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
      this.visit();
    } catch (cause) {
      this.error = failure(cause);
    } finally {
      this.progress = "";
      this.busy = false;
    }
  }
  flush() {
    return this.writes;
  }
}
