<script module lang="ts">
  import type { ActionJournalAccess, PersistedAction } from "./actionJournal.js";

  export interface ActionRecoveryJournal extends ActionJournalAccess {
    verifyCheckpoint(bytes: Uint8Array): Promise<void>;
    confirmResume(actionID: string): Promise<void>;
    pause(): Promise<void>;
    abandon(): Promise<void>;
  }
</script>

<script lang="ts">
  import { untrack } from "svelte";
  import { Button, Checkbox, Chip, Modal, Spinner } from "@kenn-io/kit-ui";
  import { APIError, type Tag } from "./api.js";
  import { encodeRecovery } from "./actionRecovery.js";
  import { runAction, type ActionProgress } from "./actionRunner.js";

  interface Props {
    session: string;
    sessionVaultID: string;
    journal: ActionRecoveryJournal;
    initialAction: PersistedAction;
    tag: Tag | null;
    onprogress: ActionProgress;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let {
    session,
    sessionVaultID,
    journal,
    initialAction,
    tag,
    onprogress,
    onclose,
    onauthfailure,
  }: Props = $props();
  let action = $state<PersistedAction>(untrack(() => initialAction));
  let confirmed = $state(false);
  let running = $state(false);
  let checking = $state(false);
  let failure = $state("");
  let notice = $state("");
  let abandonWarning = $state(false);
  let targetBatch = $state(0);
  let targetOffset = $state(0);
  let controller: AbortController | undefined;
  const batch = $derived(action.batches[targetBatch]);
  const visibleTargets = $derived(batch.members.slice(targetOffset, targetOffset + 50));

  const vaultMatches = $derived(sessionVaultID === action.vault_id);
  const completedBatches = $derived(action.batches.filter((batch) => batch.state === "complete").length);
  const remainingBatches = $derived(action.batches.length - completedBatches);
  const stateLabel = $derived(action.state === "prepared" ? "Pending" :
    action.state === "sending" ? "Running" :
    action.state[0].toUpperCase() + action.state.slice(1));
  const runnable = $derived(tag !== null && vaultMatches && action.checkpoint_verified &&
    action.state !== "stale" && action.state !== "complete" && confirmed && !running && !checking);

  async function reload(): Promise<void> {
    const stored = await journal.load();
    if (!stored) throw new Error("The recoverable action is no longer available.");
    action = stored;
    onprogress(stored);
  }

  async function saveCheckpoint(): Promise<void> {
    failure = "";
    try {
      const bytes = await encodeRecovery(action);
      const blob = new Blob([Uint8Array.from(bytes)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = `docbank-tag-action-${action.action_id}.json`;
      link.click();
      URL.revokeObjectURL(url);
      notice = "Recovery checkpoint saved. Select that saved file below before mutation.";
    } catch (cause) {
      failure = cause instanceof Error ? cause.message : String(cause);
    }
  }

  async function verifyCheckpoint(event: Event): Promise<void> {
    const input = event.currentTarget as HTMLInputElement;
    const file = input.files?.[0];
    input.value = "";
    if (!file || checking || running) return;
    checking = true;
    failure = "";
    notice = "";
    try {
      await journal.verifyCheckpoint(new Uint8Array(await file.arrayBuffer()));
      await reload();
      notice = "Recovery checkpoint verified.";
    } catch (cause) {
      failure = cause instanceof Error ? cause.message : String(cause);
    } finally {
      checking = false;
    }
  }

  async function run(): Promise<void> {
    if (!runnable) return;
    running = true;
    confirmed = false;
    failure = "";
    notice = "";
    abandonWarning = false;
    controller = new AbortController();
    try {
      await journal.confirmResume(action.action_id);
      const result = await runAction(session, journal, controller.signal, (next, receipt) => {
        action = next;
        onprogress(next, receipt);
      });
      action = result;
      onprogress(result);
    } catch (cause) {
      try { await reload(); } catch { /* Preserve the mutation error. */ }
      failure = cause instanceof Error ? cause.message : String(cause);
      if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
    } finally {
      running = false;
      controller = undefined;
    }
  }

  async function pause(): Promise<void> {
    if (!running) return;
    controller?.abort();
    try {
      await journal.pause();
      await reload();
      notice = "Action paused. An in-flight request may still be recorded before scheduling stops.";
    } catch (cause) {
      failure = cause instanceof Error ? cause.message : String(cause);
    }
  }

  async function abandon(): Promise<void> {
    if (running) return;
    failure = "";
    try {
      await journal.abandon();
      onclose();
    } catch (cause) {
      failure = cause instanceof Error ? cause.message : String(cause);
    }
  }
</script>

<Modal title="Recoverable snapshot action" tone="info" width="700px"
  maxWidth="min(700px, calc(100vw - 32px))" ariaLabel="Recoverable snapshot action"
  onclose={() => { if (!running) onclose(); }} closeOnOverlayClick={!running && !checking}>
  <div class="recovery">
    <div class="heading">
      <Chip size="sm" tone={action.state === "complete" ? "success" : action.state === "stale" || action.state === "uncertain" ? "warning" : "info"}
        uppercase={false}>{stateLabel}</Chip>
      <span>{completedBatches} completed · {remainingBatches} remaining batch{remainingBatches === 1 ? "" : "es"}</span>
    </div>

    <p class="warning">Recovery files contain private document identities, hashes, sizes, and revisions. They never contain the browser session credential.</p>

    <dl>
      <div><dt>Vault</dt><dd><code>{action.vault_id}</code></dd></div>
      <div><dt>Action</dt><dd><code>{action.action_id}</code></dd></div>
      <div><dt>Tag</dt><dd>{tag?.name ?? "Unavailable tag"} <code>{action.tag_id}</code></dd></div>
      <div><dt>Operation</dt><dd>{action.assign ? "Add" : "Remove"} “{tag?.name ?? action.tag_id}”</dd></div>
      <div><dt>Targets</dt><dd>{action.total} exact document{action.total === 1 ? "" : "s"}</dd></div>
    </dl>

    {#if !tag}
      <p role="alert">This tag is no longer available. You can save the checkpoint or abandon the action.</p>
    {/if}

    <details>
      <summary>Review exact targets</summary>
      <p>Recovery files contain instructions supplied by their creator. Checksums detect inconsistent files; they do not prove who created the file. Confirm only targets you intend to change.</p>
      <label for="action-target-batch">Target batch</label>
        <select id="action-target-batch" value={targetBatch} onchange={(event) => { targetBatch = Number(event.currentTarget.value); targetOffset = 0; }}>
          {#each action.batches as item (item.index)}
            <option value={item.index}>Batch {item.index + 1} · {item.members.length} documents</option>
          {/each}
        </select>
      <div class="target-list">
        <table aria-label="Exact action targets">
          <thead><tr><th>Node ID</th><th>Content version</th><th>SHA-256</th><th>Bytes</th><th>Expected revision</th></tr></thead>
          <tbody>{#each visibleTargets as member (member.node_id)}
            <tr><td>{member.node_id}</td><td><code>{member.content_version_id}</code></td><td><code>{member.blob_hash}</code></td><td>{member.size}</td><td>{member.revision}</td></tr>
          {/each}</tbody>
        </table>
      </div>
      <nav class="actions" aria-label="Action target pages">
        <span>{targetOffset + 1}–{targetOffset + visibleTargets.length} of {batch.members.length} in this batch</span>
        <Button disabled={targetOffset === 0} onclick={() => (targetOffset -= 50)}>Previous targets</Button>
        <Button disabled={targetOffset + 50 >= batch.members.length} onclick={() => (targetOffset += 50)}>Next targets</Button>
      </nav>
    </details>

    {#if !vaultMatches}
      <p role="alert">This fresh browser session is connected to a different vault. This action cannot run here.</p>
    {/if}

    <section aria-labelledby="checkpoint-heading">
      <h3 id="checkpoint-heading">Recovery checkpoint</h3>
      <p>Save the checkpoint, then select that saved file back. A download click alone does not unlock mutation.</p>
      <div class="actions">
        <Button disabled={running || checking} onclick={() => void saveCheckpoint()}>Save recovery checkpoint</Button>
        <label class:disabled={running || checking}>
          <span>Select the saved recovery checkpoint</span>
          <input type="file" accept="application/json,.json" disabled={running || checking}
            onchange={(event) => void verifyCheckpoint(event)} />
        </label>
      </div>
    </section>

    {#if action.state === "stale"}
      <p class="state-copy">This action is stale. Its expected revisions will not be refreshed. Create an explicit new selection and action instead.</p>
    {:else if action.state === "uncertain"}
      <p class="state-copy">The result is uncertain. Retry same operation uses every original operation identity and expected revision.</p>
    {:else if action.state === "paused"}
      <p class="state-copy">The action is paused. Resuming uses the retained original requests.</p>
    {:else if action.state === "complete"}
      <p class="state-copy">Action complete. All batches have validated receipts.</p>
    {:else if action.state === "sending" || running}
      <p class="state-copy"><Spinner size={16} /> The action is running. Stop after the current request to pause safely.</p>
    {:else}
      <p class="state-copy">The action is pending. No mutation starts before checkpoint readback and explicit confirmation.</p>
    {/if}

    {#if notice}<p role="status">{notice}</p>{/if}
    {#if failure}<p role="alert">{failure}</p>{/if}

    {#if vaultMatches && action.state !== "stale" && action.state !== "complete"}
      <div class="confirmation">
        <Checkbox checked={confirmed} disabled={!action.checkpoint_verified || running || checking}
          ariaLabel="I confirm this vault, action, tag, operation, and exact targets"
          label="I confirm this vault, action, tag, operation, and exact targets." onchange={(value) => (confirmed = value)} />
        <Button tone="info" disabled={!runnable} onclick={() => void run()}>
          {action.state === "uncertain" ? "Retry same operation" : action.state === "paused" ? "Confirm and resume action" : "Confirm and run action"}
        </Button>
        {#if running}<Button onclick={() => void pause()}>Pause after current request</Button>{/if}
      </div>
    {/if}

    <div class="abandon">
      {#if abandonWarning}
        <p role="alert">Abandoning deletes this browser’s journal. Completed or in-flight changes are not rolled back. Save the latest checkpoint first if you may need its receipts.</p>
        <Button tone="danger" disabled={running} onclick={() => void abandon()}>Abandon action without rollback</Button>
      {:else}
        <Button surface="soft" disabled={running} onclick={() => (abandonWarning = true)}>Abandon action…</Button>
      {/if}
    </div>
  </div>
  {#snippet footer()}<Button surface="soft" disabled={running || checking} onclick={onclose}>Close</Button>{/snippet}
</Modal>

<style>
  .recovery { display: grid; grid-template-columns: minmax(0, 1fr); gap: var(--space-4); }
  .recovery p, .recovery h3 { margin: 0; }
  .heading, .actions, .confirmation { display: flex; align-items: center; flex-wrap: wrap; gap: var(--space-3); }
  .heading span { color: var(--text-muted); font-size: var(--font-size-sm); }
  .warning { padding: var(--space-3); border: 1px solid var(--border-warning); border-radius: var(--radius-md); background: var(--bg-warning-soft); }
  dl { display: grid; gap: var(--space-2); margin: 0; }
  dl div { display: grid; grid-template-columns: 90px minmax(0, 1fr); gap: var(--space-3); }
  dt { color: var(--text-muted); }
  dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
  section, .abandon { display: grid; gap: var(--space-3); padding-block-start: var(--space-4); border-top: 1px solid var(--border-default); }
  h3 { font-size: var(--font-size-sm); }
  label { display: grid; gap: var(--space-2); font-size: var(--font-size-sm); font-weight: 600; }
  label.disabled { opacity: 0.6; }
  .state-copy { display: flex; align-items: center; gap: var(--space-2); }
  code { font-size: var(--font-size-xs); }
  details > * { margin-block-start: var(--space-3); }
  summary { cursor: pointer; }
  .target-list { overflow-x: auto; max-height: 300px; }
  table { width: 100%; table-layout: fixed; border-collapse: collapse; font-size: var(--font-size-xs); }
  th, td { padding: var(--space-2); text-align: left; overflow-wrap: anywhere; border-bottom: 1px solid var(--border-default); }
  @media (max-width: 640px) { dl div { grid-template-columns: 1fr; gap: var(--space-1); } }
</style>
