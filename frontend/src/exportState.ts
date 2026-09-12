import { APIError, requestResponse } from "./api.js";
import { captureSnapshotTargets, type SnapshotPage } from "./snapshots.js";
import {
  assertExportAdvance, cancelExportJob, copyExportMembers, createExportPlan,
  exportExpired, exportTicket, getExportJob, maxExportMembers, exportEmailPDFRecipes, exportOutputProblems,
  offerExportDownload, readExportEvents, safeExportBasename, sealExportSource,
  startExportJob, validateRolePolicies, validateExportOptions, sealMailboxExportSource,
  type ExportJob, type ExportMember, type ExportPlan, type ExportPreview,
  type ExportSource, type RolePolicy, type ExportOptions, type EmailPDFRecipeChoice, type OutputProblems,
} from "./exports.js";

export type ExportInput = { label: string; members: readonly ExportMember[] } | { label: string; snapshot: SnapshotPage } | { label: string; collectionID: string; total: number };
export interface ReviewedExport { plan: ExportPlan; preview: ExportPreview; basename: string; label: string }
export interface ActiveExport { plan: ExportPlan; basename: string; label: string; id: string; job?: ExportJob }
export interface ExportState {
  status: "idle" | "preparing" | "ready" | "starting" | "running" | "completed" | "canceled" | "failed" | "expired" | "disconnected" | "error";
  reviewed?: ReviewedExport;
  active?: ActiveExport;
  error?: Error;
  gap?: boolean;
  downloadOffered?: boolean;
  downloading?: boolean;
  recipes?: EmailPDFRecipeChoice[];
  problems?: OutputProblems;
  problemsLoading?: boolean;
}

// The handle lives for this browser session. Closing releases readers without
// canceling server work; reconnect always addresses this same admitted job.
export class ExportSession {
  private state: Readonly<ExportState> = { status: "idle" };
  private input?: ExportInput;
  private policies: RolePolicy[] = [];
  private options: ExportOptions = {};
  private basename = "docbank-bundle.zip";
  private sourceID = "";
  private planID = "";
  private members?: ExportMember[];
  private source?: ExportSource;
  private controller?: AbortController;
  private generation = 0;
  private disposed = false;
  private expiryTimer?: ReturnType<typeof setTimeout>;

  constructor(private readonly session: string, private readonly publish: (state: Readonly<ExportState>) => void) { publish(this.state); }

  choose(input: ExportInput, policies: RolePolicy[], basename: string, options: ExportOptions = {}): void {
    this.stop();
    // Snapshot DTOs contain JSON data. Copy through JSON so reactive browser
    // proxies cannot fail structuredClone or remain mutable through the caller.
    const copied = "members" in input ? { ...input, members: input.members.map(m => ({ ...m })) } : "snapshot" in input ? { ...input, snapshot: JSON.parse(JSON.stringify(input.snapshot)) as SnapshotPage } : { ...input };
    const changed = JSON.stringify(copied) !== JSON.stringify(this.input);
    this.input = copied;
    this.policies = policies.map(p => ({ ...p })); this.basename = basename;
    this.options = JSON.parse(JSON.stringify(options)) as ExportOptions;
    this.planID = crypto.randomUUID();
    if (changed) { this.sourceID = crypto.randomUUID(); this.members = undefined; this.source = undefined; }
    const job = this.state.active?.job;
    const status = this.state.active ? (!job || ["queued", "running"].includes(job.state) ? "disconnected" : this.state.status) : "idle";
    this.emit({ ...this.state, status, reviewed: undefined, error: undefined, problems: undefined, problemsLoading: false, ...(changed ? { recipes: undefined } : {}) });
  }

  async discoverRecipes(): Promise<void> {
    if (!this.input || this.disposed || this.state.active) return;
    const started = this.begin();
    this.emit({ ...this.state, status: "preparing", error: undefined, reviewed: undefined, recipes: undefined });
    try {
      const source = await this.prepareSource(this.input, started);
      if (!source) return;
      const recipes = await exportEmailPDFRecipes(this.session, source, started.signal);
      if (this.current(started.generation)) this.emit({ ...this.state, status: "idle", recipes });
    } catch (error) { this.fail(started.generation, error); }
  }

  private async prepareSource(input: ExportInput, started: { generation: number; signal: AbortSignal }): Promise<ExportSource | undefined> {
    if (this.source && exportExpired(this.source.expires_at)) {
      this.source = undefined; this.sourceID = crypto.randomUUID(); this.planID = crypto.randomUUID();
    }
    if (this.source) return this.source;
    if ("collectionID" in input) {
      const source = await sealMailboxExportSource(this.session, input.collectionID, input.total, this.sourceID, started.signal);
      if (!this.current(started.generation)) return;
      return this.source = source;
    }
    if (!this.members) {
      if ("snapshot" in input) {
        if (input.snapshot.total > maxExportMembers) throw new Error("Exports are limited to 100,000 documents. Refine this query and capture a new snapshot.");
        const captured = await captureSnapshotTargets(this.session, input.snapshot, started.signal);
        if (!this.current(started.generation)) return;
        this.members = copyExportMembers(captured.members);
      } else {
        this.members = copyExportMembers(input.members.map(m => ({ node_id: m.node_id, content_version_id: m.version_id, blob_hash: m.sha256, size: m.size })));
      }
    }
    const source = await sealExportSource(this.session, this.members, this.sourceID, started.signal);
    if (!this.current(started.generation)) return;
    return this.source = source;
  }

  async preview(): Promise<void> {
    if (!this.input || this.disposed) return;
    if (this.state.status === "expired") {
      this.source = undefined; this.sourceID = crypto.randomUUID(); this.planID = crypto.randomUUID();
    }
    const input = this.input, started = this.begin();
    this.emit({ ...this.state, status: "preparing", reviewed: undefined, error: undefined });
    try {
      const basename = safeExportBasename(this.basename), policies = validateRolePolicies(this.policies), options = validateExportOptions(this.options);
      const source = await this.prepareSource(input, started);
      if (!source) return;
      const reviewed = await createExportPlan(this.session, source, policies, this.planID, started.signal, options);
      const counts = reviewed.plan.counts;
      const problems = counts && (counts.unavailable || counts.unavailable_inventories) ? await exportOutputProblems(this.session, reviewed.plan, 0, started.signal) : undefined;
      if (!this.current(started.generation)) return;
      this.emit({ ...this.state, status: "ready", reviewed: { ...reviewed, basename, label: input.label }, problems, error: undefined });
      const delay = Date.parse(reviewed.plan.expires_at) - Date.now();
      if (delay >= 0 && delay < 2 ** 31) this.expiryTimer = setTimeout(() => {
        if (this.current(started.generation) && !this.state.active) this.emit({ ...this.state, status: "expired", reviewed: undefined });
      }, delay);
    } catch (error) { this.fail(started.generation, error); }
  }

  async problemPage(after: number): Promise<void> {
    const plan = this.state.active?.plan ?? this.state.reviewed?.plan;
    if (!plan || this.disposed || this.state.problemsLoading) return;
    if (this.state.active && (!this.state.active.job || ["queued", "running"].includes(this.state.active.job.state))) return;
    const started = this.begin();
    this.emit({ ...this.state, problemsLoading: true, error: undefined });
    try {
      const problems = await exportOutputProblems(this.session, plan, after, started.signal);
      if (this.current(started.generation)) this.emit({ ...this.state, problems, problemsLoading: false });
    } catch (error) {
      if (this.current(started.generation)) this.emit({ ...this.state, problemsLoading: false });
      this.fail(started.generation, error);
    }
  }

  async start(): Promise<void> {
    const reviewed = this.state.reviewed;
    if (!reviewed || this.disposed || this.state.active) return;
    if (exportExpired(reviewed.plan.expires_at)) {
      this.emit({ ...this.state, status: "expired", reviewed: undefined, error: new Error("The export plan expired. Preview again.") }); return;
    }
    const active = { plan: reviewed.plan, basename: reviewed.basename, label: reviewed.label, id: crypto.randomUUID() };
    const started = this.begin();
    this.emit({ ...this.state, status: "starting", active, error: undefined, downloadOffered: false });
    try {
      let job: ExportJob;
      try { job = await startExportJob(this.session, active.plan, active.id, started.signal); }
      catch (error) {
        if (started.signal.aborted || error instanceof APIError && error.status < 500) throw error;
        job = await getExportJob(this.session, active.plan, active.id, started.signal);
      }
      if (!this.current(started.generation)) return;
      this.accept(job);
      await this.watch(started);
    } catch (error) { this.fail(started.generation, error, true); }
  }

  async reconnect(): Promise<void> {
    const active = this.state.active;
    if (!active || this.disposed) return;
    const started = this.begin();
    this.emit({ ...this.state, status: "starting", error: undefined });
    try {
      let job: ExportJob;
      try { job = await getExportJob(this.session, active.plan, active.id, started.signal); }
      catch (error) {
        if (!(error instanceof APIError && error.status === 404) || active.job) throw error;
        job = await startExportJob(this.session, active.plan, active.id, started.signal);
      }
      if (!this.current(started.generation)) return;
      this.accept(job);
      await this.watch(started);
    } catch (error) { this.fail(started.generation, error, true); }
  }

  async cancel(): Promise<void> {
    const active = this.state.active;
    if (!active || this.disposed) return;
    const started = this.begin();
    try {
      await cancelExportJob(this.session, active.id, started.signal);
      const job = await getExportJob(this.session, active.plan, active.id, started.signal);
      if (this.current(started.generation)) this.accept(job);
    } catch (error) { this.fail(started.generation, error, true); }
  }

  async download(): Promise<void> {
    const active = this.state.active;
    if (!active?.job || active.job.state !== "completed" || this.state.downloading || this.disposed) return;
    const started = this.begin();
    this.emit({ ...this.state, downloading: true, error: undefined });
    try {
      const ticket = await exportTicket(this.session, active.plan, active.job, active.basename, started.signal);
      if (!this.current(started.generation)) {
        // A issued-but-unoffered ticket still owns an archive lease.
        await requestResponse(ticket.url.replace("/file?", "?"), this.session, { method: "DELETE" }).catch(() => undefined);
        return;
      }
      offerExportDownload(ticket, active.basename);
      this.emit({ ...this.state, downloading: false, downloadOffered: true });
    } catch (error) { this.fail(started.generation, error); }
  }

  close(): void {
    this.stop();
    const active = this.state.active;
    this.emit({ ...this.state, downloading: false, ...(active && (!active.job || ["queued", "running"].includes(active.job.state)) ? { status: "disconnected" } : {}) });
  }
  clearFinished(): void {
    if (!this.state.active?.job || !["completed", "canceled", "failed"].includes(this.state.active.job.state)) return;
    this.stop(); this.source = undefined; this.sourceID = crypto.randomUUID(); this.planID = crypto.randomUUID();
    this.emit({ status: "idle" });
  }
  dispose(): void { this.close(); this.disposed = true; }

  private async watch(started: { generation: number; signal: AbortSignal }): Promise<void> {
    const active = this.state.active;
    if (!active?.job || !["queued", "running"].includes(active.job.state)) return;
    try {
      await readExportEvents(this.session, active.plan, active.id, active.job, started.signal, (job, gap) => {
        if (this.current(started.generation)) this.accept(job, gap);
      });
    } catch (error) {
      if (!this.current(started.generation)) return;
      if (error instanceof APIError && (error.status === 401 || error.status === 410)) throw error;
      // A closed or malformed stream supplies no completion proof. Recover one
      // authoritative snapshot, and leave an explicit reconnect if still active.
      const job = await getExportJob(this.session, active.plan, active.id, started.signal);
      if (!this.current(started.generation)) return;
      this.accept(job);
      if (["queued", "running"].includes(job.state)) this.fail(started.generation, error, true);
    }
  }

  private accept(job: ExportJob, gap = false): void {
    const active = this.state.active;
    if (!active) return;
    if (active.job) assertExportAdvance(active.job, job);
    this.emit({ ...this.state, status: job.state === "queued" ? "running" : job.state as ExportState["status"], active: { ...active, job }, error: undefined, gap: this.state.gap || gap });
  }
  private fail(generation: number, cause: unknown, disconnected = false): void {
    if (!this.current(generation)) return;
    const error = cause instanceof Error ? cause : new Error("Export request failed.");
    this.emit({ ...this.state, downloading: false, status: error instanceof APIError && error.status === 410 ? "expired" : disconnected ? "disconnected" : "error", error });
  }
  private begin(): { generation: number; signal: AbortSignal } {
    this.stop(); this.controller = new AbortController();
    return { generation: this.generation, signal: this.controller.signal };
  }
  private stop(): void {
    this.generation++; this.controller?.abort(); this.controller = undefined;
    if (this.expiryTimer) clearTimeout(this.expiryTimer);
    this.expiryTimer = undefined;
  }
  private current(generation: number): boolean { return !this.disposed && generation === this.generation; }
  private emit(state: ExportState): void { if (!this.disposed) { this.state = state; this.publish(state); } }
}
