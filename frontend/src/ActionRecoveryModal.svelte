<script module lang="ts">
  import type { ActionJournalAccess, PersistedAction } from "./actionJournal.js";

  export interface ActionRecoveryJournal extends ActionJournalAccess {
    verifyCheckpoint(bytes: Uint8Array): Promise<void>;
    confirmResume(actionID: string): Promise<void>;
    pause(): Promise<void>;
    abandon(): Promise<void>;
    discardUnsent(actionID: string): Promise<void>;
  }
</script>

<script lang="ts">
  import { onDestroy, untrack } from "svelte";
  import CheckIcon from "@lucide/svelte/icons/check";
  import DownloadIcon from "@lucide/svelte/icons/download";
  import { Button, Modal, Spinner, Table } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import type { Tag } from "./generated/docbank.js";
  import { encodeRecovery } from "./actionRecovery.js";
  import { runAction, type ActionProgress } from "./actionRunner.js";
  import { documentCount, tagChangeConsequence, tagChangeDone } from "./tagChangeText.js";

  interface Props {
    session: string;
    sessionVaultID: string;
    journal: ActionRecoveryJournal;
    initialAction: PersistedAction;
    tag: Tag | null;
    disabled?: boolean;
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
    disabled = false,
    onprogress,
    onclose,
    onauthfailure,
  }: Props = $props();
  let action = $state<PersistedAction>(untrack(() => initialAction));
  let running = $state(false);
  let failure = $state("");
  let notice = $state("");
  let abandonWarning = $state(false);
  let targetBatch = $state(0);
  let targetOffset = $state(0);
  let controller: AbortController | undefined;
  let alive = true;
  onDestroy(() => {
    alive = false;
    controller?.abort();
  });
  const batch = $derived(action.batches[targetBatch]);
  const visibleTargets = $derived(batch.members.slice(targetOffset, targetOffset + 50));

  const vaultMatches = $derived(sessionVaultID === action.vault_id);
  const tagName = $derived(tag?.name ?? "deleted tag");
  const doneDocuments = $derived(
    action.batches
      .filter((item) => item.state === "complete")
      .reduce((sum, item) => sum + item.members.length, 0),
  );
  const changedDocuments = $derived(
    action.batches.reduce(
      (sum, item) => sum + (item.receipt?.nodes.filter((node) => node.changed).length ?? 0),
      0,
    ),
  );
  const finished = $derived(action.state === "complete" || action.state === "stale");
  // The journal rechecks stored progress before discarding on close.
  const untouched = $derived(
    action.state === "prepared" && action.batches.every((item) => item.state === "prepared"),
  );
  const runnable = $derived(!disabled && tag !== null && vaultMatches && !finished && !running);
  const applyLabel = $derived(
    action.state === "uncertain" ? "Retry" : untouched && doneDocuments === 0 ? "Apply" : "Resume",
  );

  async function reload(): Promise<void> {
    const stored = await journal.load();
    if (!alive) return;
    if (!stored) throw new Error("The recoverable action is no longer available.");
    action = stored;
    onprogress(stored);
  }

  // Downloads the recovery file and returns the bytes it holds; undefined when encoding failed.
  async function saveCheckpoint(): Promise<Uint8Array | undefined> {
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
      return bytes;
    } catch (cause) {
      failure = cause instanceof Error ? cause.message : String(cause);
    }
  }

  async function run(): Promise<void> {
    if (!runnable) return;
    running = true;
    failure = "";
    notice = "";
    abandonWarning = false;
    controller = new AbortController();
    try {
      // The first Apply saves the recovery file the journal requires before it sends anything.
      if (!action.checkpoint_verified) {
        const bytes = await saveCheckpoint();
        if (!bytes) return;
        await journal.verifyCheckpoint(bytes);
        await reload();
      }
      // Pressing Apply is the explicit confirmation the runner requires.
      await journal.confirmResume(action.action_id);
      const result = await runAction(session, journal, controller.signal, (next, receipt) => {
        if (!alive) return;
        action = next;
        onprogress(next, receipt);
      });
      if (!alive) return;
      action = result;
      onprogress(result);
    } catch (cause) {
      if (!alive) return;
      try {
        await reload();
      } catch {
        /* Preserve the mutation error. */
      }
      if (!alive) return;
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
      notice = "Paused. A request already sent may still finish.";
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

  async function close(): Promise<void> {
    if (running) return;
    try {
      if (untouched) await journal.discardUnsent(action.action_id);
      onclose();
    } catch (cause) {
      failure = cause instanceof Error ? cause.message : String(cause);
    }
  }
</script>

<Modal
  title="Tag all results"
  tone="info"
  width="480px"
  maxWidth="min(480px, calc(100vw - 32px))"
  ariaLabel="Tag all results"
  onclose={close}
  closeOnOverlayClick={!running}
>
  <div class="run">
    {#if action.state !== "complete"}
      <p class="consequence">{tagChangeConsequence(action.assign, tagName, action.total)}</p>
    {/if}

    {#if action.state === "complete"}
      <p class="status done" role="status">
        <CheckIcon size={14} aria-hidden="true" />
        {tagChangeDone(action.assign, tagName, changedDocuments, action.total)}
      </p>
    {:else if running || action.state === "sending"}
      <p class="status" role="status">
        <Spinner size={14} />
        {doneDocuments.toLocaleString()} of {documentCount(action.total)} done
      </p>
    {:else if action.state === "paused" || doneDocuments > 0}
      <p class="status" role="status">
        {action.state === "paused" ? "Paused · " : ""}{doneDocuments.toLocaleString()} of {documentCount(
          action.total,
        )} done
      </p>
    {/if}

    {#if !tag}<p class="alert" role="alert">This tag no longer exists.</p>{/if}
    {#if !vaultMatches}<p class="alert" role="alert">This run belongs to a different vault.</p>{/if}
    {#if action.state === "stale"}
      <p class="alert" role="alert">
        Some documents changed after this search, so tagging stopped. Changes already made stay.
      </p>
    {:else if action.state === "uncertain" && !running}
      <p class="alert" role="alert">
        The last request's result is unknown{failure ? ` (${failure})` : ""}.
      </p>
    {/if}

    {#if !untouched && !finished && !running && action.state !== "sending"}
      <p class="privacy">
        <Button size="sm" surface="soft" onclick={() => void saveCheckpoint()}
          ><DownloadIcon size={14} aria-hidden="true" /> Save recovery file</Button
        >
        Contains private document IDs and hashes.
      </p>
    {/if}

    {#if notice}<p class="note" role="status">{notice}</p>{/if}
    {#if failure && action.state !== "uncertain"}<p class="alert" role="alert">{failure}</p>{/if}

    <details>
      <summary>Review documents</summary>
      {#if action.checkpoint_verified}
        <p class="note">
          Recovery files can come from anyone. Apply only if these are the documents you expect.
        </p>
      {/if}
      <dl>
        <dt>Vault</dt>
        <dd><code>{action.vault_id}</code></dd>
        <dt>Run</dt>
        <dd><code>{action.action_id}</code></dd>
        <dt>Tag</dt>
        <dd><code>{action.tag_id}</code></dd>
      </dl>
      {#if action.batches.length > 1}
        <label class="batch-select">
          <span>Batch</span>
          <select
            value={targetBatch}
            onchange={(event) => {
              targetBatch = Number(event.currentTarget.value);
              targetOffset = 0;
            }}
          >
            {#each action.batches as item (item.index)}
              <option value={item.index}
                >{item.index + 1} · {documentCount(item.members.length)}</option
              >
            {/each}
          </select>
        </label>
      {/if}
      <div class="target-list">
        <Table
          unsorted
          ariaLabel="Documents in this run"
          class="target-table"
          zebra={false}
          scroll="page"
        >
          {#snippet header()}
            <th scope="col">ID</th><th scope="col">Content version</th><th scope="col">SHA-256</th
            ><th scope="col">Bytes</th><th scope="col">Revision</th>
          {/snippet}
          {#snippet children()}
            {#each visibleTargets as member (member.node_id)}
              <tr
                ><td>{member.node_id}</td><td><code>{member.content_version_id}</code></td><td
                  ><code>{member.blob_hash}</code></td
                ><td>{member.size}</td><td>{member.revision}</td></tr
              >
            {/each}
          {/snippet}
        </Table>
      </div>
      {#if batch.members.length > 50}
        <nav class="pager" aria-label="Document pages">
          <span
            >{targetOffset + 1}–{targetOffset + visibleTargets.length} of {batch.members
              .length}</span
          >
          <Button size="sm" disabled={targetOffset === 0} onclick={() => (targetOffset -= 50)}
            >Previous</Button
          >
          <Button
            size="sm"
            disabled={targetOffset + 50 >= batch.members.length}
            onclick={() => (targetOffset += 50)}>Next</Button
          >
        </nav>
      {/if}
    </details>

    {#if abandonWarning}
      <div class="confirm" role="alert">
        <span>Stop this run? Changes already made stay.</span>
        <Button size="sm" tone="danger" disabled={running} onclick={() => void abandon()}
          >Discard run</Button
        >
        <Button size="sm" surface="soft" onclick={() => (abandonWarning = false)}>Keep</Button>
      </div>
    {/if}
  </div>
  {#snippet footer()}
    {#if !untouched && action.state !== "complete" && !abandonWarning}
      <span class="footer-start">
        <Button
          size="sm"
          surface="soft"
          tone={action.state === "stale" ? "danger" : "neutral"}
          disabled={running}
          onclick={() => (abandonWarning = true)}>Discard run…</Button
        >
      </span>
    {/if}
    {#if action.state === "complete"}
      <Button tone="info" surface="solid" onclick={onclose}>Done</Button>
    {:else}
      <Button surface="soft" disabled={running} onclick={close}
        >{untouched ? "Cancel" : "Finish later"}</Button
      >
      {#if running}
        <Button onclick={() => void pause()}>Pause</Button>
      {:else if action.state !== "stale"}
        <Button tone="info" surface="solid" disabled={!runnable} onclick={() => void run()}
          >{applyLabel}</Button
        >
      {/if}
    {/if}
  {/snippet}
</Modal>

<style>
  .run {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: var(--space-4);
  }
  .run p {
    margin: 0;
  }
  .consequence {
    color: var(--text-primary);
    font-weight: var(--font-weight-semibold, 600);
  }
  .status,
  .privacy {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
  }
  .status {
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }
  .status.done {
    color: var(--accent-green);
  }
  .alert {
    color: var(--accent-red);
    font-size: var(--font-size-sm);
  }
  .note,
  .privacy {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .confirm,
  .pager {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
    font-size: var(--font-size-sm);
  }
  .confirm span {
    margin-inline-end: auto;
  }
  .footer-start {
    margin-inline-end: auto;
  }
  details {
    font-size: var(--font-size-sm);
  }
  details > * + * {
    margin-block-start: var(--space-3);
  }
  summary {
    cursor: pointer;
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }
  dl {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr);
    gap: var(--space-1) var(--space-3);
    margin-block-end: 0;
  }
  dt {
    color: var(--text-muted);
  }
  dd {
    margin: 0;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  code {
    font-size: var(--font-size-xs);
  }
  .batch-select {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .target-list {
    overflow-x: auto;
    max-height: 240px;
  }
  .target-list :global(.target-table .kit-table) {
    table-layout: fixed;
    font-size: var(--font-size-xs);
  }
  .target-list :global(.target-table th),
  .target-list :global(.target-table td) {
    padding: var(--space-2);
    text-align: left;
    overflow-wrap: anywhere;
    border-bottom: 1px solid var(--border-default);
  }
</style>
