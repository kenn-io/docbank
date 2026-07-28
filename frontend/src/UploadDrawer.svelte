<script lang="ts">
  import { onDestroy } from "svelte";
  import FileUpIcon from "@lucide/svelte/icons/file-up";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import Trash2Icon from "@lucide/svelte/icons/trash-2";
  import XIcon from "@lucide/svelte/icons/x";
  import {
    Button,
    Chip,
    DetailDrawer,
    EmptyState,
    IconButton,
    Spinner,
  } from "@kenn-io/kit-ui";
  import { APIError, type Node, type UploadReceipt } from "./api.js";
  import {
    hashFile,
    type TransferProgress,
    UploadChannelError,
    type UploadTransport,
  } from "./upload.js";
  import { formatBytes } from "./format.js";

  type UploadState =
    | "ready"
    | "hashing"
    | "uploading"
    | "added"
    | "skipped"
    | "failed"
    | "unconfirmed"
    | "cancelled";

  interface UploadItem {
    id: number;
    file: File;
    state: UploadState;
    progress: TransferProgress;
    hash?: string;
    error?: string;
    receipt?: UploadReceipt;
  }

  interface Props {
    channel: UploadTransport;
    directory: Node;
    disabledReason?: string;
    onclose: () => void;
    oncomplete: () => void | Promise<void>;
    onauthfailure: (cause: unknown) => void;
  }

  let {
    channel, directory, disabledReason = "", onclose, oncomplete, onauthfailure,
  }: Props = $props();
  let input: HTMLInputElement;
  let items = $state<UploadItem[]>([]);
  let running = $state(false);
  let controller: AbortController | null = null;
  let nextID = 1;

  const pending = $derived(
    items.filter((item) =>
      ["ready", "failed", "unconfirmed", "cancelled"].includes(item.state),
    ).length,
  );
  const completed = $derived(
    items.filter((item) => item.state === "added" || item.state === "skipped")
      .length,
  );
  const failures = $derived(
    items.filter((item) => item.state === "failed").length,
  );

  onDestroy(() => controller?.abort());

  function chooseFiles(): void {
    input.click();
  }

  function addFiles(event: Event): void {
    const selected = Array.from((event.currentTarget as HTMLInputElement).files ?? []);
    items = [
      ...items,
      ...selected.map((file) => ({
        id: nextID++,
        file,
        state: "ready" as const,
        progress: { processed: 0, total: file.size },
      })),
    ];
    input.value = "";
  }

  function updateItem(id: number, patch: Partial<UploadItem>): void {
    items = items.map((item) => (item.id === id ? { ...item, ...patch } : item));
  }

  function removeItem(id: number): void {
    if (!running) items = items.filter((item) => item.id !== id);
  }

  function progressFor(id: number, progress: TransferProgress): void {
    updateItem(id, { progress });
  }

  async function start(): Promise<void> {
    if (running || pending === 0) return;
    const active = new AbortController();
    controller = active;
    running = true;
    let refreshNeeded = false;
    let authFailed = false;

    for (const queued of items) {
      if (!["ready", "failed", "unconfirmed", "cancelled"].includes(queued.state)) continue;
      if (active.signal.aborted) break;
      let transmissionStarted = false;
      updateItem(queued.id, {
        state: "hashing",
        error: undefined,
        receipt: undefined,
        progress: { processed: 0, total: queued.file.size },
      });
      try {
        const hash = await hashFile(
          queued.file,
          active.signal,
          (progress) => progressFor(queued.id, progress),
        );
        updateItem(queued.id, {
          state: "uploading",
          hash,
          progress: { processed: 0, total: queued.file.size },
        });
        const receipt = await channel.uploadFile(
          directory.id,
          queued.file,
          hash,
          active.signal,
          (progress) => {
            transmissionStarted = true;
            refreshNeeded = true;
            progressFor(queued.id, progress);
          },
        );
        updateItem(queued.id, {
          state: receipt.status,
          receipt,
          progress: { processed: queued.file.size, total: queued.file.size },
        });
      } catch (cause) {
        if (cause instanceof DOMException && cause.name === "AbortError") {
          if (transmissionStarted) {
            updateItem(queued.id, {
              state: "unconfirmed",
              error:
                "Upload outcome is unconfirmed. Refreshing the folder; retry this file to converge safely.",
            });
          } else {
            updateItem(queued.id, {
              state: "cancelled",
              error: "Cancelled before upload began.",
            });
          }
          break;
        }
        if (cause instanceof APIError && cause.status === 401) {
          updateItem(queued.id, { state: "failed", error: cause.message });
          authFailed = true;
          onauthfailure(cause);
          break;
        }
        if (transmissionStarted && !(cause instanceof APIError)) {
          updateItem(queued.id, {
            state: "unconfirmed",
            error:
              "Upload outcome is unconfirmed. Refreshing the folder; retry this file after running `docbank web` again.",
          });
          break;
        }
        updateItem(queued.id, {
          state: "failed",
          error: cause instanceof Error ? cause.message : String(cause),
        });
        if (cause instanceof UploadChannelError) break;
      }
    }

    if (controller === active) {
      controller = null;
      running = false;
      if (refreshNeeded && !authFailed) await oncomplete();
    }
  }

  function cancel(): void {
    controller?.abort();
  }

  function close(): void {
    controller?.abort();
    onclose();
  }

  function percentage(item: UploadItem): number {
    if (item.progress.total === 0) return item.state === "ready" ? 0 : 100;
    return Math.min(
      100,
      Math.round((item.progress.processed * 100) / item.progress.total),
    );
  }

  function stateLabel(item: UploadItem): string {
    switch (item.state) {
      case "ready":
        return "Ready";
      case "hashing":
        return "Hashing";
      case "uploading":
        return "Uploading";
      case "added":
        return "Added";
      case "skipped":
        return "Already present";
      case "failed":
        return "Failed";
      case "cancelled":
        return "Cancelled";
      case "unconfirmed":
        return "Unconfirmed";
    }
  }
</script>

<DetailDrawer width="min(720px, 100vw)" ariaLabel="Upload documents" onclose={close}>
  {#snippet header()}
    <div class="drawer-heading">
      <div>
        <span>VERIFIED IMPORT</span>
        <strong>Upload documents</strong>
        <small>{directory.path ?? `Directory id:${directory.id}`}</small>
      </div>
      <IconButton size="sm" ariaLabel="Close upload documents" onclick={close}>
        <XIcon size="14" aria-hidden="true" />
      </IconButton>
    </div>
  {/snippet}

  <div class="upload">
    <section class="destination" aria-label="Upload destination">
      <div><span>DESTINATION</span><strong>{directory.path ?? "Moved directory"}</strong></div>
      <Chip size="xs" tone="muted" uppercase={false}>id:{directory.id}</Chip>
    </section>

    <p class="contract">
      Docbank reads each file twice: once locally to declare its SHA-256
      identity, then once to stream it. Authority is granted only when the
      daemon independently computes the same hash and size. Bytes travel only
      over the upload channel proved by this daemon; a broken channel is never
      reconnected.
    </p>
    {#if disabledReason}
      <p class="channel-error" role="alert">{disabledReason}</p>
    {/if}

    <input
      bind:this={input}
      class="kit-sr-only"
      type="file"
      multiple
      onchange={addFiles}
      aria-label="Choose local files"
    />

    <div class="controls">
      <Button
        size="sm"
        surface="soft"
        disabled={running || Boolean(disabledReason)}
        onclick={chooseFiles}
      >
        <FileUpIcon size="14" aria-hidden="true" />
        Choose files
      </Button>
      {#if running}
        <Button size="sm" tone="danger" surface="soft" onclick={cancel}>
          <XIcon size="14" aria-hidden="true" />
          Cancel current upload
        </Button>
      {:else if pending > 0 && !disabledReason}
        <Button size="sm" tone="info" surface="solid" onclick={() => void start()}>
          <FileUpIcon size="14" aria-hidden="true" />
          Upload {pending} file{pending === 1 ? "" : "s"}
        </Button>
      {/if}
    </div>

    {#if items.length === 0}
      <EmptyState
        title="Choose files from this device"
        description="Files are copied into the current Docbank directory. Local source files are never changed or removed."
      >
        {#snippet icon()}<FileUpIcon size="22" />{/snippet}
      </EmptyState>
    {:else}
      <div class="queue-heading">
        <strong>{items.length} selected</strong>
        <span>
          {completed} accepted
          {#if failures > 0} · {failures} failed{/if}
        </span>
      </div>
      <ul class="queue" aria-live="polite">
        {#each items as item (item.id)}
          <li>
            <div class="file-row">
              <div class="file-authority">
                <strong>{item.file.name}</strong>
                <span>{formatBytes(item.file.size)} · {item.file.type || "application/octet-stream"}</span>
              </div>
              <div class="file-actions">
                <Chip
                  size="xs"
                  tone={item.state === "failed"
                    ? "danger"
                    : item.state === "added" || item.state === "skipped"
                      ? "success"
                      : item.state === "cancelled" || item.state === "unconfirmed"
                        ? "warning"
                        : "neutral"}
                  uppercase={false}
                >
                  {#if item.state === "hashing" || item.state === "uploading"}
                    <Spinner size={11} />
                  {/if}
                  {stateLabel(item)}
                </Chip>
                {#if !running && item.state !== "added" && item.state !== "skipped"}
                  <IconButton
                    size="sm"
                    ariaLabel={`Remove ${item.file.name} from upload`}
                    onclick={() => removeItem(item.id)}
                  >
                    <Trash2Icon size="13" aria-hidden="true" />
                  </IconButton>
                {/if}
              </div>
            </div>

            {#if item.state === "hashing" || item.state === "uploading"}
              <div class="progress-copy">
                <span>{stateLabel(item)}</span>
                <span>{formatBytes(item.progress.processed)} / {formatBytes(item.progress.total)}</span>
              </div>
              <div
                class="progress"
                role="progressbar"
                aria-label={`${stateLabel(item)} ${item.file.name}`}
                aria-valuemin="0"
                aria-valuemax={item.progress.total}
                aria-valuenow={item.progress.processed}
              >
                <span style={`width: ${percentage(item)}%`}></span>
              </div>
            {:else if item.error}
              <p class="item-error" role="alert">{item.error}</p>
            {:else if item.receipt}
              <p class="receipt">
                {item.receipt.status === "added"
                  ? `Created node ${item.receipt.node.id}, revision ${item.receipt.node.revision}.`
                  : `Matched existing node ${item.receipt.node.id}.`}
                <code>{item.receipt.computed_hash}</code>
              </p>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}

    {#if completed > 0 && !running}
      <aside class="outcome">
        <RefreshCwIcon size="15" aria-hidden="true" />
        <span>
          {completed} file{completed === 1 ? "" : "s"} accepted with matching
          server receipts. The current folder has been refreshed.
        </span>
      </aside>
    {/if}
  </div>
</DetailDrawer>

<style>
  .drawer-heading,
  .destination,
  .controls,
  .queue-heading,
  .file-row,
  .file-actions,
  .progress-copy,
  .outcome {
    display: flex;
    align-items: center;
  }

  .drawer-heading,
  .destination,
  .queue-heading,
  .file-row {
    justify-content: space-between;
  }

  .drawer-heading {
    width: 100%;
    min-width: 0;
    gap: var(--space-4);
  }

  .drawer-heading > div,
  .destination > div,
  .file-authority {
    display: flex;
    min-width: 0;
    flex-direction: column;
  }

  .drawer-heading span,
  .destination span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-bold);
    letter-spacing: var(--letter-spacing-label, 0.04em);
  }

  .drawer-heading strong {
    color: var(--text-primary);
    font-size: var(--font-size-lg);
  }

  .drawer-heading small {
    overflow: hidden;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .upload {
    display: grid;
    gap: var(--space-4);
    padding: var(--space-5);
  }

  .destination {
    gap: var(--space-3);
    padding: var(--space-4);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    background: var(--bg-inset);
  }

  .destination strong,
  .file-authority strong {
    overflow-wrap: anywhere;
  }

  .contract {
    margin: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }

  .channel-error {
    padding: var(--space-3);
    border: 1px solid color-mix(in srgb, var(--color-danger, #dc2626) 45%, transparent);
    border-radius: var(--radius-md);
    margin: 0;
    background: color-mix(in srgb, var(--color-danger, #dc2626) 10%, transparent);
    color: var(--text-primary);
    font-size: var(--font-size-sm);
  }

  .controls {
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .queue-heading {
    gap: var(--space-3);
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
  }

  .queue-heading span {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .queue {
    display: grid;
    max-height: min(52vh, 520px);
    padding: 0;
    margin: 0;
    gap: var(--space-2);
    list-style: none;
    overflow-y: auto;
  }

  .queue li {
    display: grid;
    padding: var(--space-3);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    gap: var(--space-2);
  }

  .file-row {
    min-width: 0;
    gap: var(--space-3);
  }

  .file-authority span,
  .progress-copy,
  .receipt,
  .item-error {
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .file-actions {
    flex-shrink: 0;
    gap: var(--space-1);
  }

  .progress-copy {
    justify-content: space-between;
    gap: var(--space-2);
  }

  .progress {
    width: 100%;
    height: 5px;
    overflow: hidden;
    border-radius: 999px;
    background: var(--bg-inset);
  }

  .progress > span {
    display: block;
    height: 100%;
    border-radius: inherit;
    background: var(--accent);
    transition: width 100ms linear;
  }

  .item-error,
  .receipt {
    display: grid;
    margin: 0;
    gap: var(--space-1);
    line-height: 1.4;
  }

  .item-error {
    color: var(--danger-text);
  }

  .receipt code {
    overflow-wrap: anywhere;
  }

  .outcome {
    align-items: flex-start;
    padding: var(--space-3);
    border: 1px solid var(--success-border);
    border-radius: var(--radius-md);
    background: var(--success-bg);
    color: var(--success-text);
    font-size: var(--font-size-sm);
    gap: var(--space-2);
  }

  @media (max-width: 640px) {
    .upload {
      padding: var(--space-4);
    }

    .file-row {
      align-items: flex-start;
    }

    .destination {
      align-items: flex-start;
    }
  }
</style>
