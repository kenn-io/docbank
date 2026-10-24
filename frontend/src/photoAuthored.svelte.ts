import * as api from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import type { HiddenAccess } from "./hiddenAccess.svelte.js";

type PreparedFile = api.PhotoPreparedTarget & api.PhotoAuthoredEditTarget;

export class PhotoUndoState {
  receiptID = $state("");
  summary = $state("");
}

export class PhotoAuthoredEditor {
  onXMPChanged?: (receipt?: api.PhotoAuthoredReceipt, tags?: api.Tag[]) => Promise<void>;
  async undoSidecarImport(fileID: string) {
    const receiptID = this.targets.find((target) => target.file_id === fileID)?.xmp
      ?.import_receipt_id;
    if (!receiptID || this.busy || this.loading || this.dirty || this.concealed) return;
    await this.undo(async (receipt, tags) => {
      await this.onXMPChanged?.(receipt, tags);
    }, receiptID);
  }

  async resolveXMP(fileID: string, choice: api.PhotoXMPResolveRequestChoice) {
    if (this.busy || this.loading || this.concealed || this.dirty) return;
    const expected = this.targets.find((target) => target.file_id === fileID)?.xmp;
    if (!expected || expected.state !== "conflict") return;
    const write = this.beginWrite();
    this.error = "";
    try {
      const receipt = await this.request(() =>
        api.resolvePhotoXMP(fileID, { conflict_id: expected.conflict_id!, choice }, this.options()),
      );
      if (this.controller.signal.aborted) return;
      this.endWrite(write);
      await this.reload();
      await this.publishXMPKeywords(receipt || undefined);
    } catch (cause) {
      if (!this.controller.signal.aborted) {
        this.error =
          cause instanceof APIError && [404, 409, 412].includes(cause.status)
            ? "The photo or sidecar changed. Reopen the photo and choose again."
            : this.failure(cause);
      }
    } finally {
      this.endWrite(write);
    }
  }
  asset = $state<api.PhotoAsset>();
  /** Every prepared file once, by file_id. The edit selection and the stack each name the files they use. */
  private held = $state<Record<string, PreparedFile>>({});
  private editIDs = $state.raw(new Set<string>());
  private stackIDs = $state.raw(new Set<string>());
  private editFiles = $derived(this.members(this.editIDs));
  private stackFiles = $derived(this.members(this.stackIDs));
  /** The edit selection. */
  get targets() {
    return this.editFiles;
  }
  set targets(targets: PreparedFile[]) {
    this.editIDs = this.hold(targets);
    this.prune();
  }
  /** Every stack member, prepared so one stack decision checks every member. */
  get stackTargets() {
    return this.stackFiles;
  }
  private setStack(targets: PreparedFile[]) {
    this.stackIDs = this.hold(targets.map((target) => this.stackTarget(target)));
    this.prune();
  }
  private stackTarget(target: PreparedFile) {
    return target.xmp ? target : { ...target, xmp: this.held[target.file_id]?.xmp };
  }
  /** Each held file once, whichever scopes use it. */
  get prepared() {
    return Object.values(this.held);
  }
  private members(ids: Set<string>) {
    return [...ids].flatMap((id) => this.held[id] ?? []);
  }
  private hold(targets: PreparedFile[]) {
    for (const target of targets) this.held[target.file_id] = target;
    return new Set(targets.map((target) => target.file_id));
  }
  private prune() {
    for (const id of Object.keys(this.held))
      if (!this.editIDs.has(id) && !this.stackIDs.has(id)) delete this.held[id];
  }
  stackLoading = $state(false);
  canChooseStackRepresentative(assetID: string) {
    const rejected = new Set(
      this.stackTargets
        .filter((target) => target.values.flag === "reject")
        .map((target) => target.asset_id),
    );
    for (const target of this.stackTargets)
      if (target.values.flag === "pick") rejected.delete(target.asset_id);
    return (
      this.stackTargets.some((target) => target.asset_id === assetID) &&
      (!rejected.has(assetID) || this.stackTargets.every((target) => rejected.has(target.asset_id)))
    );
  }
  tags = $state<api.Tag[]>([]);
  tagsError = $state("");
  tagsLoading = $state(false);
  tagsTotal = $state(0);
  fileID = $state("");
  patch = $state<api.PhotoAuthoredPatch>({});
  rotationDelta = $state<api.PhotoAuthoredEditRequest["rotation_delta"]>();
  keywords = $state<api.PhotoKeywordEdit[]>([]);
  loading = $state(false);
  validating = $state(false);
  concealed = $state(false);
  busy = $state(false);
  conflict = $state(false);
  membershipChanged = $state(false);
  error = $state("");
  notice = $state("");
  noticeTone = $state<"success" | "warning">("success");
  get receiptID() {
    return this.undoState.receiptID;
  }
  set receiptID(value: string) {
    this.undoState.receiptID = value;
  }
  inspectedID = $state("");
  loadTags?: () => Promise<void>;
  loadMoreTags?: () => Promise<void>;
  catalog(tags: api.Tag[]) {
    const assigned = new Set(this.targets.flatMap((target) => target.tags)),
      supplied = new Set(tags.map((tag) => tag.id));
    this.tags = [
      ...tags,
      ...this.tags.filter((tag) => assigned.has(tag.id) && !supplied.has(tag.id)),
    ];
    void this.resolveAssignedTags();
  }
  private pendingTags = new Map<string, Promise<void>>();
  private xmpTimer?: ReturnType<typeof setTimeout>;
  private xmpRead?: AbortController;
  private xmpAttempts = 0;
  private xmpTerminal = false;
  private watchXMP(restart = false) {
    if (restart) {
      this.stopXMP();
      this.xmpAttempts = 0;
      this.xmpTerminal = false;
    }
    if (
      this.controller.signal.aborted ||
      this.conflict ||
      this.concealed ||
      this.xmpTerminal ||
      !this.asset ||
      !this.targets.some((target) => target.xmp)
    ) {
      this.stopXMP();
      return;
    }
    if (!this.xmpTimer && !this.xmpRead) {
      this.xmpTimer = setTimeout(
        () => {
          this.xmpTimer = undefined;
          void this.refreshXMP();
        },
        Math.min(2000 * 2 ** this.xmpAttempts, 30000),
      );
    }
  }
  private stopXMP() {
    clearTimeout(this.xmpTimer);
    this.xmpTimer = undefined;
    this.xmpRead?.abort();
    this.xmpRead = undefined;
  }
  // refreshXMP owns the polling schedule: every exit, including a skipped read, plans the next one.
  async refreshXMP() {
    try {
      await this.readXMP();
    } finally {
      this.watchXMP();
    }
  }
  private async readXMP() {
    if (this.busy || this.loading || this.concealed || this.conflict || this.xmpRead) return;
    const targets = this.targets;
    const assetIDs = this.xmpAssets(targets);
    if (!assetIDs.length) return;
    this.xmpAttempts = Math.min(this.xmpAttempts + 1, 4);
    const write = this.writes;
    const controller = (this.xmpRead = new AbortController());
    const signal = AbortSignal.any([controller.signal, this.controller.signal]);
    try {
      const prepared = await this.request(() =>
        api.preparePhotoAuthored(
          { asset_ids: assetIDs, hidden: this.scope?.hidden, skip_unavailable: true },
          this.options(signal),
        ),
      );
      if (signal.aborted || this.busy || write !== this.writes || targets !== this.targets) return;
      const scope = targets.filter((target) => assetIDs.includes(target.asset_id));
      const changed = this.acceptXMPStatus(prepared, scope);
      if (changed) await this.publishXMPKeywords(undefined, [...scope, ...prepared.targets]);
    } catch (cause) {
      if (
        !signal.aborted &&
        cause instanceof APIError &&
        cause.status >= 400 &&
        cause.status < 500 &&
        ![408, 429].includes(cause.status)
      ) {
        this.xmpTerminal = true;
        this.failure(cause);
      }
    } finally {
      if (this.xmpRead === controller) this.xmpRead = undefined;
    }
  }
  private acceptXMPStatus(prepared: api.PhotoAuthoredPreparation, scope: typeof this.targets) {
    const latest = new Map(prepared.targets.map((target) => [target.file_id, target]));
    const asset = prepared.assets.find((asset) => asset.id === this.asset?.id);
    const unchanged =
      prepared.targets.length === scope.length &&
      scope.every((target) => {
        const fresh = latest.get(target.file_id);
        return fresh?.revision === target.revision && fresh.node_revision === target.node_revision;
      }) &&
      (!this.asset ||
        (!!asset &&
          (
            [
              "kind",
              "display_file_id",
              "display_override_file_id",
              "hidden_at",
              "excluded_at",
            ] as const
          ).every((key) => asset[key] === this.asset![key])));
    const changed =
      !unchanged ||
      scope.some((target) => {
        const fresh = latest.get(target.file_id);
        return !!fresh && JSON.stringify(fresh.xmp) !== JSON.stringify(target.xmp);
      });
    const accept =
      !this.dirty &&
      prepared.targets.length === scope.length &&
      scope.every((target) => latest.has(target.file_id));
    if (!unchanged && !accept) this.conflict = true;
    this.targets = this.targets.map((target) => {
      const fresh = latest.get(target.file_id);
      if (!fresh) return target;
      if (accept) return fresh;
      return {
        ...target,
        xmp: fresh.xmp,
        asset_revision: unchanged && asset ? fresh.asset_revision : target.asset_revision,
      };
    });
    if (accept && asset && this.asset)
      this.asset = {
        ...asset,
        files: asset.files.filter((file) =>
          this.targets.some((target) => target.file_id === file.id),
        ),
      };
    else if (unchanged && asset && this.asset)
      this.asset = { ...this.asset, revision: asset.revision };
    if (changed) this.xmpAttempts = 0;
    return changed;
  }
  // While Metadata shows a target, its XMP status is observed in every state, so an outside
  // import or conflict appears without reopening. The targets fence the refreshed status.
  private xmpAssets(targets: typeof this.targets) {
    return [...new Set(targets.filter((target) => target.xmp).map((target) => target.asset_id))];
  }
  private async resolveAssignedTags() {
    const known = new Set(this.tags.map((tag) => tag.id));
    const missing = [...new Set(this.targets.flatMap((target) => target.tags))].filter(
      (id) => !known.has(id) && !this.pendingTags.has(id),
    );
    for (const id of missing) {
      const request = this.request(() => api.getTag(id, this.options()))
        .then((tag) => {
          if (
            this.pendingTags.get(id) === request &&
            !this.controller.signal.aborted &&
            this.targets.some((target) => target.tags.includes(id)) &&
            !this.tags.some((value) => value.id === id)
          )
            this.tags = [...this.tags, tag];
        })
        .catch((cause) => {
          if (this.pendingTags.get(id) === request && !this.controller.signal.aborted)
            this.tagsError = this.failure(cause);
        })
        .finally(() => {
          if (this.pendingTags.get(id) === request) this.pendingTags.delete(id);
        });
      this.pendingTags.set(id, request);
    }
    await Promise.all(this.pendingTags.values());
  }
  // Either choice can import unrelated outside changes.
  private async publishXMPKeywords(
    receipt?: api.PhotoAuthoredReceipt,
    targets: api.PhotoPreparedTarget[] = [],
  ) {
    await this.loadTags?.();
    const tags = await this.refreshKeywords(receipt ?? targets.flatMap((target) => target.tags));
    if (receipt || tags.length) await this.onXMPChanged?.(receipt, tags);
    else await this.onXMPChanged?.();
  }
  private refreshKeywords(source: api.PhotoAuthoredReceipt | string[]) {
    return this.refreshKeywordIDs(
      Array.isArray(source) ? source : source.keywords?.map((change) => change.tag_id),
    );
  }
  private async refreshKeywordIDs(ids: string[] = []) {
    const changed = new Set(ids);
    if (!changed.size) return [];
    for (const id of changed) this.pendingTags.delete(id);
    const refreshed = await Promise.all(
      [...changed].map(async (id) => {
        try {
          const tag = await this.request(() => api.getTag(id, this.options()));
          if (this.controller.signal.aborted) return;
          if (this.tags.some((value) => value.id === id))
            this.tags = this.tags.map((value) => (value.id === id ? tag : value));
          else if (this.targets.some((target) => target.tags.includes(id)))
            this.tags = [...this.tags, tag];
          return tag;
        } catch (cause) {
          if (!this.controller.signal.aborted) this.tagsError = this.failure(cause);
        }
      }),
    );
    return refreshed.filter((tag): tag is api.Tag => !!tag);
  }
  private controller = new AbortController();
  private read = new AbortController();
  private stackRead = new AbortController();
  private stackKey = $state("");
  stackError = $state("");
  private scope?: api.PhotoAuthoredPrepareRequest;
  constructor(
    private session: string,
    private onfailure: (cause: unknown) => void,
    readonly undoState = new PhotoUndoState(),
    private readonly hiddenAccess?: HiddenAccess,
  ) {}
  private request<T>(request: () => Promise<T>) {
    return this.hiddenAccess ? this.hiddenAccess.run(request) : request();
  }
  private options(signal = this.controller.signal) {
    return { session: this.session, signal };
  }
  private failure(cause: unknown) {
    this.onfailure(cause);
    return cause instanceof Error ? cause.message : String(cause);
  }
  // A write releases busy before its list refresh, so a later write may own busy by the time this one settles.
  private writes = 0;
  private beginWrite() {
    this.busy = true;
    return ++this.writes;
  }
  private endWrite(write: number) {
    if (write === this.writes) this.busy = false;
  }
  canChangeTarget() {
    if (this.loading || this.stackLoading) {
      this.unavailable();
      return false;
    }
    if (!this.dirty && !this.busy) return true;
    this.unavailable(
      this.busy
        ? "An edit is in progress. Wait for it to finish."
        : "Apply or clear your metadata edits before changing photos.",
    );
    return false;
  }
  reset() {
    this.stopXMP();
    this.read.abort();
    this.stackRead.abort();
    this.held = {};
    this.editIDs = new Set();
    this.stackIDs = new Set();
    this.stackKey = "";
    this.stackError = "";
    this.stackLoading = false;
    this.asset = undefined;
    this.fileID = "";
    this.patch = {};
    this.rotationDelta = undefined;
    this.keywords = [];
    this.scope = undefined;
    this.error = "";
    this.conflict = false;
    this.membershipChanged = false;
    this.loading = false;
    this.validating = false;
    this.concealed = false;
  }
  conceal() {
    this.stopXMP();
    this.read.abort();
    this.loading = false;
    this.concealed = true;
    this.validating = true;
  }
  revalidate(eligible: Set<string>) {
    const targets = this.targets.filter((target) => eligible.has(target.asset_id));
    if (targets.length !== this.targets.length) {
      this.fileID = "";
      if (this.dirty || this.busy) {
        this.membershipChanged = false;
        this.conflict = true;
      }
    }
    this.targets = targets;
    if (this.asset && !eligible.has(this.asset.id)) this.asset = undefined;
    this.validating = false;
    this.concealed = false;
    this.watchXMP(true);
  }
  async prepare(scope: api.PhotoAuthoredPrepareRequest, reload = false, keepError = false) {
    this.read.abort();
    const controller = (this.read = new AbortController());
    const signal = AbortSignal.any([controller.signal, this.controller.signal]);
    this.loading = true;
    if (!keepError) this.error = "";
    const previous = reload ? new Set(this.targets.map((target) => target.file_id)) : undefined;
    try {
      const prepared = await this.request(() =>
        api.preparePhotoAuthored(scope, this.options(signal)),
      );
      if (!Array.isArray(prepared.assets) || !Array.isArray(prepared.targets))
        throw new Error("Metadata response was incomplete. Reload metadata.");
      if (signal.aborted) return;
      if (
        previous &&
        (prepared.targets.length !== previous.size ||
          [...previous].some((id) => !prepared.targets.some((target) => target.file_id === id)))
      ) {
        this.membershipChanged = true;
        if (!prepared.targets.some((target) => target.file_id === this.fileID)) this.fileID = "";
      }
      this.targets = previous
        ? prepared.targets.filter((target) => previous.has(target.file_id))
        : prepared.targets;
      const asset = prepared.assets.length === 1 ? prepared.assets[0] : undefined;
      this.asset = asset
        ? {
            ...asset,
            files: asset.files.filter((file) =>
              this.targets.some((target) => target.file_id === file.id),
            ),
          }
        : undefined;
      this.scope = scope;
      this.conflict = this.membershipChanged;
      this.watchXMP(true);
      void this.resolveAssignedTags();
      if (!this.tags.length && !this.tagsLoading && !this.tagsError)
        void this.loadTags?.().catch((cause) => {
          if (!signal.aborted) this.tagsError = this.failure(cause);
        });
    } catch (cause) {
      if (!signal.aborted) this.error = this.failure(cause);
    } finally {
      if (!signal.aborted) this.loading = false;
    }
  }
  async reload(keepError = false) {
    if (this.busy || this.loading) return;
    // Stack members prepare again once the stack is actionable.
    this.stackRead.abort();
    this.setStack([]);
    this.stackKey = "";
    this.stackError = "";
    this.stackLoading = false;
    if (!this.targets.length) {
      this.conflict = false;
      if (!keepError) this.error = "";
      return;
    }
    await this.prepare(
      {
        asset_ids: [...new Set(this.targets.map((target) => target.asset_id))],
        hidden: this.scope?.hidden,
        skip_unavailable: true,
      },
      true,
      keepError,
    );
  }
  reviewMembers() {
    this.membershipChanged = false;
    this.conflict = false;
  }
  private selectedFiles = $derived(
    this.targets.filter((target) => !this.fileID || target.file_id === this.fileID),
  );
  private keywordCounts = $derived.by(() => {
    const counts = new Map<string, number>();
    for (const file of this.selectedFiles)
      for (const tag of file.tags) counts.set(tag, (counts.get(tag) ?? 0) + 1);
    return counts;
  });
  get files() {
    return this.selectedFiles;
  }
  get count() {
    return new Set(this.targets.map((target) => target.asset_id)).size;
  }
  get dirty() {
    return (
      Object.keys(this.patch).length > 0 ||
      this.rotationDelta !== undefined ||
      this.keywords.length > 0
    );
  }
  value(field: keyof api.PhotoAuthoredPatch) {
    if (this.patch[field] !== undefined) return this.patch[field];
    const files = this.files;
    if (!files.length || files.some((file) => file.values[field] !== files[0]!.values[field]))
      return undefined;
    return files[0]!.values[field];
  }
  stage(patch: api.PhotoAuthoredPatch, keywords: api.PhotoKeywordEdit[] = []) {
    this.patch = { ...this.patch, ...patch };
    if (patch.rotation !== undefined) this.rotationDelta = undefined;
    for (const keyword of keywords)
      this.keywords = [
        ...this.keywords.filter((value) => value.tag_id !== keyword.tag_id),
        keyword,
      ];
  }
  clearDraft() {
    this.patch = {};
    this.rotationDelta = undefined;
    this.keywords = [];
    if (!this.targets.length) {
      this.conflict = false;
      this.membershipChanged = false;
    }
  }
  member(fileID: string) {
    if (!this.dirty) this.fileID = fileID;
  }
  keywordState(tagID: string) {
    const staged = this.keywords.find((keyword) => keyword.tag_id === tagID);
    if (staged) return staged.assign ? "assigned" : "";
    const count = this.keywordCounts.get(tagID) ?? 0;
    return count === 0 ? "" : count === this.files.length ? "assigned" : "mixed";
  }
  unavailable(
    message = this.concealed && !this.validating
      ? "Retry the photo refresh before editing metadata."
      : this.loading || this.validating
        ? "Metadata is loading. Try again when it finishes."
        : "An edit is in progress. Try the shortcut again when it finishes.",
  ) {
    this.noticeTone = "warning";
    this.notice = message;
  }
  private accept(receipt: api.PhotoAuthoredReceipt) {
    this.noticeTone = "success";
    for (const target of this.accepted(this.prepared, receipt)) this.held[target.file_id] = target;
  }
  private accepted(targets: PreparedFile[], receipt: api.PhotoAuthoredReceipt) {
    this.watchXMP(true);
    return targets.map((target) => {
      const representative = receipt.representatives?.find(
        (change) => change.asset_id === target.asset_id,
      );
      if (representative) target = { ...target, asset_revision: representative.after_revision };
      const after = receipt.after.find((file) => file.file_id === target.file_id);
      if (!after) return target;
      const tags = new Set(target.tags);
      for (const change of receipt.keywords ?? [])
        if (change.node_id === target.node_id) {
          if (change.after) tags.add(change.tag_id);
          else tags.delete(change.tag_id);
        }
      const nodeRevision =
        receipt.keyword_nodes?.find((node) => node.node_id === target.node_id)?.after_revision ??
        target.node_revision + (after.revision - target.revision);
      return {
        ...target,
        revision: after.revision,
        node_revision: nodeRevision,
        values: after.values,
        tags: [...tags],
        xmp: target.xmp ? { state: "pending" as const } : undefined,
      };
    });
  }
  async edit(
    scope: api.PhotoAlbumMembersRequest,
    patch: api.PhotoAuthoredPatch,
    hidden: boolean,
    refresh: (receipt: api.PhotoAuthoredReceipt, tags: api.Tag[]) => Promise<void>,
    {
      keywords,
      rotationDelta,
      viewed,
      beforeStage,
      beforeWrite,
    }: {
      keywords?: api.PhotoKeywordEdit[];
      rotationDelta?: api.PhotoAuthoredEditRequest["rotation_delta"];
      viewed?: { content_version_id: string };
      beforeStage?: (
        targets: PhotoAuthoredEditor["targets"],
      ) => { error?: string; fileID?: string } | void;
      beforeWrite?: () => Promise<boolean>;
    } = {},
  ) {
    if (this.conflict) {
      this.unavailable("Reload metadata before editing.");
      return;
    }
    if (this.busy || this.loading || this.stackLoading || this.concealed) {
      this.unavailable();
      return;
    }
    if (this.controller.signal.aborted) return;
    if (!this.targets.length) {
      await this.prepare({ ...scope, hidden });
      if (this.error) return;
    }
    if (this.conflict) return;
    if (!this.targets.length) return;
    const staging = beforeStage?.(this.targets);
    if (staging?.error) {
      this.error = staging.error;
      this.conflict = true;
      return;
    }
    if (staging?.fileID) this.fileID = staging.fileID;
    if (viewed) {
      this.targets = this.targets.map((target) => ({
        ...target,
        content_version_id: viewed.content_version_id,
      }));
    }
    const draft = viewed && {
      patch: this.patch,
      rotationDelta: this.rotationDelta,
      keywords: this.keywords,
    };
    this.stage(patch, keywords);
    if (rotationDelta !== undefined) {
      const current = { ...this.patch };
      delete current.rotation;
      this.patch = current;
      this.rotationDelta = rotationDelta;
    }
    if (!this.dirty) return;
    const write = this.beginWrite();
    this.error = "";
    this.notice = "";
    try {
      const edits = [
        ...Object.keys(this.patch).map((field) => (field === "label" ? "color label" : field)),
        ...(this.rotationDelta !== undefined ? ["rotation"] : []),
        ...(this.keywords.length ? ["keywords"] : []),
      ];
      if (beforeWrite && !(await beforeWrite())) return;
      const receipt = await this.request(() =>
        api.editPhotoAuthored(
          {
            targets: this.files.map(
              ({
                asset_id,
                asset_revision,
                file_id,
                revision,
                node_revision,
                content_version_id,
              }) => ({
                asset_id,
                asset_revision,
                file_id,
                revision,
                node_revision,
                content_version_id,
              }),
            ),
            patch: this.patch,
            rotation_delta: this.rotationDelta,
            keywords: this.keywords,
          },
          this.options(),
        ),
      );
      this.accept(receipt);
      this.patch = {};
      this.rotationDelta = undefined;
      this.keywords = [];
      if (!receipt.receipt_id) {
        this.notice = "No changes to apply";
        return;
      }
      this.receiptID = receipt.receipt_id;
      this.undoState.summary = `${receipt.after.length} file${receipt.after.length === 1 ? "" : "s"} updated: ${edits.join(", ")}`;
      const tags = await this.refreshKeywords(receipt);
      this.endWrite(write);
      await refresh(receipt, tags);
    } catch (cause) {
      if (!this.controller.signal.aborted) {
        this.error = this.failure(cause);
        if (cause instanceof APIError && cause.errors.length) this.error = cause.errors.join("; ");
        this.conflict = cause instanceof APIError && [404, 409, 412].includes(cause.status);
        if (this.conflict && draft) {
          this.patch = draft.patch;
          this.rotationDelta = draft.rotationDelta;
          this.keywords = draft.keywords;
        }
      }
    } finally {
      this.endWrite(write);
    }
  }
  needsStack(assetIDs: string[], hidden: boolean) {
    return this.stackKey !== JSON.stringify([hidden, assetIDs]);
  }
  /** Prepare complete stack membership independently of the browser selection. */
  async prepareStack(assetIDs: string[], hidden: boolean, retry = false) {
    const key = JSON.stringify([hidden, assetIDs]);
    if (this.controller.signal.aborted || (key === this.stackKey && !retry)) return;
    this.stackKey = key;
    this.stackRead.abort();
    const controller = (this.stackRead = new AbortController());
    const signal = AbortSignal.any([controller.signal, this.controller.signal]);
    this.setStack([]);
    this.stackLoading = true;
    this.stackError = "";
    this.error = "";
    try {
      const prepared = await this.request(() =>
        api.preparePhotoAuthored(
          { asset_ids: assetIDs, hidden, stack: true },
          this.options(signal),
        ),
      );
      if (!signal.aborted) this.setStack(prepared.targets);
    } catch (cause) {
      if (!signal.aborted) {
        this.stackError = this.error = this.failure(cause);
        this.conflict = cause instanceof APIError && [404, 409, 412].includes(cause.status);
      }
    } finally {
      if (!signal.aborted) this.stackLoading = false;
    }
  }
  /**
   * Decide one stack against the revisions this editor already holds. Each
   * observed row's asset revision, display file, and content version must
   * still match, so a stale view keeps its selection and offers Reload.
   */
  async decideStack(
    request: Omit<api.PhotoStackDecisionRequest, "targets">,
    refresh: (receipt: api.PhotoAuthoredReceipt, tags: api.Tag[]) => Promise<void>,
    {
      observed,
      beforeWrite,
    }: {
      observed: Pick<
        api.PhotoBrowseRow,
        "asset_id" | "revision" | "display_file_id" | "content_version_id" | "flag" | "rating"
      >[];
      beforeWrite?: () => Promise<boolean>;
    },
  ) {
    if (this.conflict) {
      this.unavailable("Reload metadata before editing.");
      return false;
    }
    if (this.busy || this.loading || this.stackLoading || this.concealed) {
      this.unavailable();
      return false;
    }
    if (this.dirty) {
      this.unavailable("Apply or clear your metadata edits before deciding a stack.");
      return false;
    }
    if (this.controller.signal.aborted) return false;
    if (this.stackKey !== JSON.stringify([request.hidden ?? false, request.asset_ids])) {
      this.unavailable();
      return false;
    }
    const targets = this.stackTargets;
    if (
      request.decision === "representative" &&
      !this.canChooseStackRepresentative(request.current_id)
    )
      return false;
    if (request.asset_ids.some((id) => !targets.some((target) => target.asset_id === id))) {
      this.unavailable();
      return false;
    }
    const stale = observed.some((row) => {
      const files = targets.filter((target) => target.asset_id === row.asset_id);
      return (
        files.length > 0 &&
        (files.some((target) => target.asset_revision !== row.revision) ||
          !files.some(
            (target) =>
              target.file_id === row.display_file_id &&
              target.display_file_id === row.display_file_id &&
              target.current_content_version_id === row.content_version_id &&
              target.values.flag === row.flag &&
              target.values.rating === row.rating,
          ))
      );
    });
    if (stale) {
      this.error = "This photo changed. Reload the photo before deciding.";
      this.conflict = true;
      return false;
    }
    const write = this.beginWrite();
    this.error = "";
    this.notice = "";
    try {
      if (beforeWrite && !(await beforeWrite())) return false;
      const receipt = await this.request(() =>
        api.decidePhotoStack(
          {
            ...request,
            targets: targets.map(
              ({ asset_id, asset_revision, file_id, revision, node_revision }) => {
                const row = observed.find((item) => item.display_file_id === file_id);
                return {
                  asset_id,
                  asset_revision,
                  file_id,
                  revision,
                  node_revision,
                  ...(row ? { content_version_id: row.content_version_id } : {}),
                };
              },
            ),
          },
          this.options(),
        ),
      );
      this.accept(receipt);
      if (!receipt.receipt_id) {
        this.notice = "No changes to apply";
        return true;
      }
      this.receiptID = receipt.receipt_id;
      this.undoState.summary = receipt.after.length ? "Stack updated" : "Representative chosen";
      this.endWrite(write);
      await refresh(receipt, []);
      return true;
    } catch (cause) {
      if (!this.controller.signal.aborted) {
        this.error = this.failure(cause);
        if (cause instanceof APIError && cause.errors.length) this.error = cause.errors.join("; ");
        this.conflict = cause instanceof APIError && [404, 409, 412].includes(cause.status);
      }
    } finally {
      this.endWrite(write);
    }
    return false;
  }
  async undo(
    refresh: (receipt: api.PhotoAuthoredReceipt, tags: api.Tag[]) => Promise<void>,
    receiptID = this.receiptID,
  ) {
    if (this.busy || this.loading || this.stackLoading || this.concealed) {
      this.unavailable();
      return;
    }
    if (!receiptID || this.controller.signal.aborted) return;
    const write = this.beginWrite();
    this.error = "";
    try {
      const receipt = await this.request(() => api.undoPhotoAuthored(receiptID, this.options()));
      this.accept(receipt);
      if (this.receiptID === receiptID) {
        this.receiptID = "";
        this.undoState.summary = "";
      }
      this.notice = receipt.receipt_id ? "Edit undone" : "No changes to undo";
      if (!receipt.receipt_id) return;
      const tags = await this.refreshKeywords(receipt);
      this.endWrite(write);
      await refresh(receipt, tags);
    } catch (cause) {
      if (!this.controller.signal.aborted) {
        this.error =
          cause instanceof APIError && cause.code === "stale_revision"
            ? "This photo changed. This edit can no longer be undone."
            : this.failure(cause);
      }
    } finally {
      this.endWrite(write);
    }
  }
  dispose() {
    this.stopXMP();
    this.controller.abort();
    this.read.abort();
    this.stackRead.abort();
  }
}
