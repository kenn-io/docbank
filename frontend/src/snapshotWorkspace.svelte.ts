import * as generated from "./generated/docbank.js";
import type { Tag } from "./generated/docbank.js";
import { APIError } from "./api-transport.js";
import type { BatchTagReceipt } from "./batch-tags.js";
import { ActionJournal, type PersistedAction } from "./actionJournal.js";
import { prepareAction, decodeRecovery } from "./actionRecovery.js";
import { readActionVaultID } from "./actionRunner.js";
import { SnapshotSession, type SnapshotState } from "./snapshotState.js";
import { captureSnapshotTargets, type SnapshotOptions, type SnapshotRow } from "./snapshots.js";
import type { SnapshotReceiptOverlays } from "./snapshotOverlays.js";
import type { SnapshotActionChoice } from "./SnapshotActions.svelte";
import type { Query } from "./query.js";

export class SnapshotWorkspace {
  state = $state<Readonly<SnapshotState>>({ status: "idle", offset: 0 });
  private controller: SnapshotSession | undefined;
  epoch = 0;
  selectedID = $state<number | undefined>();
  selection = $state<Set<number>>(new Set());
  overlays = $state<SnapshotReceiptOverlays>({});
  actionsOpen = $state(false);
  actionBusy = $state(false);
  private actionController: AbortController | undefined;
  private actionGeneration = 0;
  actionError = $state("");
  recoveryJournal = $state<ActionJournal | null>(null);
  recoveryAction = $state<PersistedAction | null>(null);
  recoveryTag = $state<Tag | null>(null);
  recoveryVaultID = $state("");
  private recoverySnapshotEpoch = 0;
  private recoverySnapshotID: string | undefined;

  constructor(
    private readonly getSession: () => string,
    private readonly onfailure: (cause: unknown) => void,
    private readonly onreceipt: (receipt: BatchTagReceipt) => void,
  ) {}

  reset(): void {
    this.invalidateActions();
    this.controller?.dispose();
    this.controller = undefined;
    this.state = { status: "idle", offset: 0 };
    this.selectedID = undefined;
    this.selection = new Set();
    this.overlays = {};
  }

  run(query: Query, options: SnapshotOptions): Promise<void> {
    this.invalidateActions();
    return this.snapshotSession().run(query, options);
  }

  private invalidateActions(): void {
    this.epoch++;
    this.cancelAction();
    this.closeRecovery();
  }

  private snapshotSession(): SnapshotSession {
    if (this.controller) return this.controller;
    this.controller = new SnapshotSession(this.getSession(), (next) => {
      const prior = this.state;
      this.state = next;
      const page = next.page;
      if (page !== prior.page) this.selection = new Set();
      if (next.firstPage?.snapshot_id !== prior.firstPage?.snapshot_id) this.overlays = {};
      this.selectedID = page?.rows.some((row) => row.node_id === this.selectedID)
        ? this.selectedID
        : page?.rows[0]?.node_id;
      if (next.error instanceof APIError && next.error.status === 401) this.onfailure(next.error);
    });
    return this.controller;
  }

  page(direction: "previous" | "next"): void {
    void this.controller?.page(direction);
  }

  async navigateDocument(direction: "previous" | "next"): Promise<void> {
    const page = this.state.page;
    const index = page?.rows.findIndex((row) => row.node_id === this.selectedID) ?? -1;
    if (!page || index < 0 || this.state.status !== "ready") return;
    const target = direction === "next" ? index + 1 : index - 1;
    if (target >= 0 && target < page.rows.length) {
      this.selectedID = page.rows[target]?.node_id;
      return;
    }
    const acceptedSnapshot = page.snapshot_id;
    if (!await this.controller?.page(direction)) return;
    const nextPage = this.state.page;
    if (!nextPage || nextPage.snapshot_id !== acceptedSnapshot || nextPage.rows.length === 0) return;
    this.selectedID = direction === "next"
      ? nextPage.rows[0]?.node_id
      : nextPage.rows.at(-1)?.node_id;
  }

  toggleSelection(row: SnapshotRow, checked: boolean): void {
    const next = new Set(this.selection);
    if (checked) next.add(row.node_id);
    else next.delete(row.node_id);
    this.selection = next;
  }

  selectVisible(checked = true): void {
    this.selection = checked && this.state.page
      ? new Set(this.state.page.rows.map((row) => row.node_id))
      : new Set();
  }

  closeRecovery(): void {
    this.recoveryJournal = null;
    this.recoveryAction = null;
    this.recoveryTag = null;
    this.recoveryVaultID = "";
  }

  handleRecoveryProgress(action: Readonly<PersistedAction>, receipt?: BatchTagReceipt): void {
    if (this.recoverySnapshotEpoch !== this.epoch || this.recoverySnapshotID !== this.state.firstPage?.snapshot_id ||
        this.recoveryAction?.action_id !== action.action_id) return;
    this.recoveryAction = action;
    if (receipt && this.recoverySnapshotID) this.onreceipt(receipt);
  }

  cancelAction(): void {
    this.actionGeneration++;
    this.actionController?.abort();
    this.actionController = undefined;
    this.actionBusy = false;
    this.actionError = "";
    this.actionsOpen = false;
  }

  private beginAction() {
    const session = this.getSession();
    const request = ++this.actionGeneration;
    const epoch = this.epoch;
    const snapshotID = this.state.firstPage?.snapshot_id;
    const controller = new AbortController();
    this.actionController = controller;
    this.actionBusy = true;
    this.actionError = "";
    return { session, signal: controller.signal,
      current: () => request === this.actionGeneration && session === this.getSession() && !controller.signal.aborted &&
        epoch === this.epoch && snapshotID === this.state.firstPage?.snapshot_id };
  }

  private async showRecovery(
    journal: ActionJournal,
    action: PersistedAction,
    vaultID: string,
    operation: ReturnType<SnapshotWorkspace["beginAction"]>,
  ): Promise<void> {
    if (!operation.current()) return;
    const currentTag = await generated.getTag(action.tag_id, { session: operation.session }).catch((cause) => {
      if (cause instanceof APIError && cause.status === 404) return null;
      throw cause;
    });
    if (!operation.current()) return;
    this.recoverySnapshotEpoch = this.epoch;
    this.recoverySnapshotID = this.state.firstPage?.snapshot_id;
    this.recoveryJournal = journal;
    this.recoveryAction = action;
    this.recoveryTag = currentTag;
    this.recoveryVaultID = vaultID;
    this.actionsOpen = false;
  }

  async startAction(choice: SnapshotActionChoice): Promise<void> {
    if (this.actionBusy || this.state.status !== "ready") return;
    this.actionError = "";
    const firstPage = this.state.firstPage;
    if (!firstPage || firstPage.total === 0) return;
    const operation = this.beginAction();
    try {
      // Complete enumeration and hash/byte verification happen before random
      // operation identities are prepared or anything is persisted.
      const targets = await captureSnapshotTargets(operation.session, firstPage, operation.signal, this.overlays);
      if (!operation.current()) return;
      const vaultID = await readActionVaultID(operation.session);
      if (!operation.current()) return;
      const journal = await ActionJournal.open(vaultID);
      const prepared = await prepareAction(vaultID, targets, choice.tagID, choice.assign);
      if (!operation.current()) return;
      await journal.prepare(prepared, operation.signal);
      const action = await journal.load();
      if (!action) throw new Error("The prepared action was not retained in durable storage.");
      await this.showRecovery(journal, action, vaultID, operation);
    } catch (cause) {
      if (!operation.current()) return;
      if (cause instanceof APIError && cause.status === 401) this.onfailure(cause);
      else this.actionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (operation.current()) {
        this.actionBusy = false;
        this.actionController = undefined;
      }
    }
  }

  async importAction(bytes: Uint8Array): Promise<void> {
    if (this.actionBusy) return;
    const operation = this.beginAction();
    try {
      const prepared = await decodeRecovery(bytes);
      if (!operation.current()) return;
      const vaultID = await readActionVaultID(operation.session);
      if (!operation.current()) return;
      if (prepared.vault_id !== vaultID) throw new Error("The recovery action belongs to a different vault.");
      const journal = await ActionJournal.open(vaultID);
      if (!operation.current()) return;
      await journal.prepare(prepared, operation.signal);
      await journal.verifyCheckpoint(bytes);
      const action = await journal.load();
      if (!action) throw new Error("The imported action was not retained in durable storage.");
      await this.showRecovery(journal, action, vaultID, operation);
    } catch (cause) {
      if (!operation.current()) return;
      if (cause instanceof APIError && cause.status === 401) this.onfailure(cause);
      else this.actionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (operation.current()) {
        this.actionBusy = false;
        this.actionController = undefined;
      }
    }
  }

  async resumeAction(): Promise<void> {
    if (this.actionBusy) return;
    const operation = this.beginAction();
    try {
      const vaultID = await readActionVaultID(operation.session);
      if (!operation.current()) return;
      const journal = await ActionJournal.open(vaultID);
      const action = await journal.load();
      if (!action) throw new Error("No retained action is available for this vault. Select a recovery file instead.");
      await this.showRecovery(journal, action, vaultID, operation);
    } catch (cause) {
      if (!operation.current()) return;
      if (cause instanceof APIError && cause.status === 401) this.onfailure(cause);
      else this.actionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (operation.current()) {
        this.actionBusy = false;
        this.actionController = undefined;
      }
    }
  }

  async abandonAction(): Promise<void> {
    if (this.actionBusy) return;
    const operation = this.beginAction();
    try {
      const vaultID = await readActionVaultID(operation.session);
      if (!operation.current()) return;
      await ActionJournal.abandon(vaultID);
      if (!operation.current()) return;
      this.actionsOpen = false;
    } catch (cause) {
      if (!operation.current()) return;
      if (cause instanceof APIError && cause.status === 401) this.onfailure(cause);
      else this.actionError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (operation.current()) {
        this.actionBusy = false;
        this.actionController = undefined;
      }
    }
  }

}
