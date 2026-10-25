<script lang="ts">
  import { reportAction, reportFailure } from "./action-events.js";
  import { onDestroy } from "svelte";
  import FileUpIcon from "@lucide/svelte/icons/file-up";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, Chip, IconButton, Spinner } from "@kenn-io/kit-ui";
  import DrawerShell from "./DrawerShell.svelte";
  import "./toolForm.css";
  import { APIError } from "./api-transport.js";
  import { type Node } from "./generated/docbank.js";
  import {
    hashFile,
    type TransferProgress,
    UploadChannelError,
    type UploadTransport,
  } from "./upload.js";
  import LocalFilePicker from "./LocalFilePicker.svelte";
  import { localFilePath, type PickerKind } from "./localFiles.js";
  import { uploadParent } from "./uploadFolders.js";
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
    error?: string;
  }

  interface Props {
    session: string;
    channel: UploadTransport;
    directory: Node;
    disabledReason?: string;
    initialPicker?: PickerKind;
    onclose: () => void;
    oncomplete: () => void | Promise<void>;
    onauthfailure: (cause: unknown) => void;
  }

  let {
    session,
    channel,
    directory,
    disabledReason = "",
    initialPicker,
    onclose,
    oncomplete,
    onauthfailure,
  }: Props = $props();
  const directories = new Map<string, number>();
  let items = $state<UploadItem[]>([]);
  let running = $state(false);
  let controller: AbortController | null = null;
  let nextID = 1;

  const pending = $derived(
    items.filter((item) => ["ready", "failed", "unconfirmed", "cancelled"].includes(item.state))
      .length,
  );
  const completed = $derived(
    items.filter((item) => item.state === "added" || item.state === "skipped").length,
  );
  const failures = $derived(items.filter((item) => item.state === "failed").length);

  onDestroy(() => controller?.abort());

  function addFiles(selected: File[]): void {
    items = [
      ...items,
      ...selected.map((file) => ({
        id: nextID++,
        file,
        state: "ready" as const,
        progress: { processed: 0, total: file.size },
      })),
    ];
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
    if (running || pending === 0 || disabledReason) return;
    const active = new AbortController();
    controller = active;
    running = true;
    let refreshNeeded = false;
    let added = false;
    let failedCause: unknown;
    let authFailed = false;

    for (const queued of items) {
      if (!["ready", "failed", "unconfirmed", "cancelled"].includes(queued.state)) continue;
      if (active.signal.aborted) break;
      let transmissionStarted = false;
      updateItem(queued.id, {
        state: "hashing",
        error: undefined,
        progress: { processed: 0, total: queued.file.size },
      });
      try {
        const hash = await hashFile(queued.file, active.signal, (progress) =>
          progressFor(queued.id, progress),
        );
        const parentID = await uploadParent(
          queued.file,
          directory.id,
          directories,
          session,
          active.signal,
          () => {
            refreshNeeded = true;
          },
        );
        active.signal.throwIfAborted();
        updateItem(queued.id, {
          state: "uploading",
          progress: { processed: 0, total: queued.file.size },
        });
        const receipt = await channel.uploadFile(
          parentID,
          queued.file,
          hash,
          active.signal,
          (progress) => {
            transmissionStarted = true;
            refreshNeeded = true;
            progressFor(queued.id, progress);
          },
        );
        refreshNeeded = true;
        added ||= receipt.status === "added";
        updateItem(queued.id, {
          state: receipt.status,
          progress: { processed: queued.file.size, total: queued.file.size },
        });
      } catch (cause) {
        if (cause instanceof DOMException && cause.name === "AbortError") {
          if (transmissionStarted) {
            updateItem(queued.id, {
              state: "unconfirmed",
              error:
                "Upload could not be confirmed. Retry this file to check whether it was added.",
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
            error: "Upload could not be confirmed. Run `docbank web` again, then retry this file.",
          });
          failedCause ??= cause;
          break;
        }
        updateItem(queued.id, {
          state: "failed",
          error: cause instanceof Error ? cause.message : String(cause),
        });
        failedCause ??= cause;
        if (cause instanceof UploadChannelError) break;
      }
    }

    if (added) reportAction(session, "document_added");
    if (failedCause) reportFailure(session, "add", failedCause);
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
    return Math.min(100, Math.round((item.progress.processed * 100) / item.progress.total));
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

{#snippet uploadFooter()}
  {#if running}
    <Button tone="danger" surface="soft" onclick={cancel}>
      <XIcon size="14" aria-hidden="true" />
      Cancel current upload
    </Button>
  {:else}
    <Button
      tone="info"
      surface="solid"
      disabled={pending === 0 || Boolean(disabledReason)}
      onclick={() => void start()}
    >
      <FileUpIcon size="14" aria-hidden="true" />
      {pending > 0 ? `Upload ${pending} file${pending === 1 ? "" : "s"}` : "Upload"}
    </Button>
  {/if}
{/snippet}

<DrawerShell
  title={`Upload to ${directory.path ?? directory.name}`}
  ariaLabel="Upload documents"
  closeLabel="Close upload documents"
  onclose={close}
  footer={items.length ? uploadFooter : undefined}
>
  <div class="tool-body">
    {#if disabledReason}
      <p class="tool-error" role="alert">{disabledReason}</p>
    {/if}

    <div class="picker" class:picker-empty={items.length === 0}>
      {#if items.length > 0}
        <span
          >{items.length} file{items.length === 1 ? "" : "s"}
          <span class="tool-muted"
            >· {completed} complete{#if failures > 0}
              · {failures} failed{/if}</span
          ></span
        >
      {/if}
      <div class="tool-row-actions">
        <LocalFilePicker
          disabled={running || Boolean(disabledReason)}
          initialKind={initialPicker}
          onchoose={addFiles}
        />
      </div>
    </div>

    {#if items.length > 0}
      <section class="tool-section" aria-label="Upload queue">
        <ul class="queue tool-list" aria-live="polite">
          {#each items as item (item.id)}
            <li>
              <div class="file-row">
                <div class="tool-row-main">
                  <strong title={localFilePath(item.file)}>{localFilePath(item.file)}</strong>
                  {#if item.state === "hashing" || item.state === "uploading"}
                    <span
                      >{formatBytes(item.progress.processed)} of {formatBytes(
                        item.progress.total,
                      )}</span
                    >
                  {:else}
                    <span>{formatBytes(item.file.size)}</span>
                  {/if}
                </div>
                <div class="tool-row-actions">
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
                      title="Remove"
                      onclick={() => removeItem(item.id)}
                    >
                      <XIcon size="14" aria-hidden="true" />
                    </IconButton>
                  {/if}
                </div>
              </div>

              {#if item.state === "hashing" || item.state === "uploading"}
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
                <p class="tool-error" role="alert">{item.error}</p>
              {/if}
            </li>
          {/each}
        </ul>
      </section>
    {/if}
  </div>
</DrawerShell>

<style>
  .picker {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
  }

  .picker-empty {
    justify-content: center;
    min-height: 160px;
    padding: var(--space-7);
    border: 1px dashed var(--border-default);
    border-radius: var(--radius-lg);
  }

  .queue > li {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: var(--space-3);
  }

  .file-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-5);
    min-width: 0;
  }

  .progress {
    width: 100%;
    height: 4px;
    overflow: hidden;
    border-radius: 999px;
    background: var(--bg-inset);
  }

  .progress > span {
    display: block;
    height: 100%;
    border-radius: inherit;
    background: var(--accent-blue);
    transition: width 100ms linear;
  }
</style>
