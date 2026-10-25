<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import MailIcon from "@lucide/svelte/icons/mail";
  import { Button, Chip, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import DrawerShell from "./DrawerShell.svelte";
  import FileChooser from "./FileChooser.svelte";
  import "./toolForm.css";
  import { APIError, sessionEmpty } from "./api-transport.js";
  import { randomUUID } from "./crypto.js";
  import type { Node } from "./generated/docbank.js";
  import { formatBytes } from "./format.js";
  import {
    mailboxJSON,
    uploadMailboxArchive,
    type MailboxChannel,
    type MailboxContainer,
    type MailboxJob,
    type MailboxPreview,
  } from "./mailbox.js";

  interface Props {
    session: string;
    channel: MailboxChannel;
    directory: Node;
    onclose: () => void;
    oncomplete: () => void | Promise<void>;
    onauthfailure: (cause: unknown) => void;
    onexport?: (collectionID: string, total: number) => void;
  }
  let { session, channel, directory, onclose, oncomplete, onauthfailure, onexport }: Props =
    $props();
  let file = $state<File | null>(null),
    dialect = $state("mboxrd"),
    busy = $state(false),
    error = $state(""),
    stage = $state("");
  let processed = $state(0),
    total = $state(0),
    containerID = $state("");
  let jobID = "";
  let container = $state<MailboxContainer | null>(null),
    preview = $state<MailboxPreview | null>(null),
    job = $state<MailboxJob | null>(null),
    prior = $state<MailboxJob[]>([]);
  let controller: AbortController | null = null;
  const lifecycle = new AbortController();
  let refreshing = false;
  onMount(() => {
    void refreshJobs();
  });
  const active = $derived(
    [job, ...prior].some((item) => item && ["running", "queued"].includes(item.state)),
  );
  $effect(() => {
    if (!active) return;
    const timer = setInterval(() => void refreshJobs(), 1000);
    return () => clearInterval(timer);
  });
  onDestroy(() => {
    controller?.abort();
    lifecycle.abort();
  });
  function fail(cause: unknown): void {
    if (cause instanceof DOMException && cause.name === "AbortError") return;
    error = cause instanceof Error ? cause.message : String(cause);
    if (cause instanceof APIError && cause.status === 401) onauthfailure(cause);
  }
  async function refreshJobs(): Promise<void> {
    if (refreshing || lifecycle.signal.aborted) return;
    refreshing = true;
    try {
      const items = await mailboxJSON<MailboxJob[]>(
        session,
        "/jobs?limit=20",
        undefined,
        lifecycle.signal,
      );
      if (lifecycle.signal.aborted) return;
      prior = items.toSorted((a, b) => b.started_at.localeCompare(a.started_at));
      if (job) {
        const previous = job.state;
        job =
          items.find((item) => item.id === job?.id) ??
          (await mailboxJSON<MailboxJob>(
            session,
            `/jobs/${encodeURIComponent(job.id)}`,
            undefined,
            lifecycle.signal,
          ));
        if (["running", "queued"].includes(previous) && !["running", "queued"].includes(job.state))
          await oncomplete();
      } else if (!file && prior.length) job = prior[0];
    } catch (cause) {
      fail(cause);
    } finally {
      refreshing = false;
    }
  }
  function choose(event: Event): void {
    file = (event.currentTarget as HTMLInputElement).files?.[0] ?? null;
    container = null;
    preview = null;
    containerID = "";
    jobID = "";
    job = null;
    error = "";
  }
  async function upload(): Promise<void> {
    if (!file || busy) return;
    busy = true;
    error = "";
    controller = new AbortController();
    if (!containerID) containerID = randomUUID();
    try {
      container = await uploadMailboxArchive(
        session,
        channel,
        file,
        containerID,
        controller.signal,
        (label, p) => {
          stage = label;
          processed = p.processed;
          total = p.total;
        },
      );
      stage = "Previewing messages";
      preview = await mailboxJSON<MailboxPreview>(
        session,
        `/containers/${encodeURIComponent(container.id)}/preview`,
        { dialect },
        controller.signal,
      );
      stage = "Source verified";
    } catch (cause) {
      fail(cause);
    } finally {
      busy = false;
      controller = null;
    }
  }
  async function start(): Promise<void> {
    if (!container || !preview || busy) return;
    busy = true;
    error = "";
    if (!jobID) jobID = randomUUID();
    try {
      job = await mailboxJSON<MailboxJob>(
        session,
        "/jobs",
        {
          id: jobID,
          container_id: container.id,
          container_sha256: container.sha256,
          settings: { dialect, destination_id: directory.id, label_tags: {} },
        },
        lifecycle.signal,
      );
      preview = null;
      await refreshJobs();
    } catch (cause) {
      fail(cause);
    } finally {
      busy = false;
    }
  }
  async function resume(continuation: boolean): Promise<void> {
    if (!job) return;
    try {
      job = await mailboxJSON<MailboxJob>(
        session,
        `/jobs/${encodeURIComponent(job.id)}/resume`,
        {
          request: {
            id: job.id,
            container_id: job.container_id,
            container_sha256: job.container_sha256,
            settings: job.settings,
          },
          continuation,
        },
        lifecycle.signal,
      );
    } catch (cause) {
      fail(cause);
    }
  }
  async function cancel(): Promise<void> {
    controller?.abort();
    try {
      if (job) {
        await mailboxJSON(
          session,
          `/jobs/${encodeURIComponent(job.id)}/cancel`,
          {},
          lifecycle.signal,
        );
        await refreshJobs();
      } else if (containerID && !container) {
        await sessionEmpty(`/api/v1/mailbox/containers/${encodeURIComponent(containerID)}`, {
          session,
          method: "DELETE",
        });
        containerID = "";
        stage = "Upload canceled";
      }
    } catch (cause) {
      fail(cause);
    }
  }
</script>

<DrawerShell
  title={`Import mailbox to ${directory.path ?? "current folder"}`}
  ariaLabel="Import mailbox archive"
  closeLabel="Close mailbox import"
  {onclose}
>
  <div class="tool-body">
    <section class="tool-section" aria-label="Mailbox source">
      <div class="tool-grid">
        <div class="tool-field tool-span-4">
          <span class="tool-label">Archive</span>
          <FileChooser
            accept=".mbox,.zip"
            label="Choose MBOX or Takeout ZIP"
            {file}
            disabled={busy}
            icon={MailIcon}
            onchange={choose}
          />
        </div>
        <div class="tool-field tool-span-2">
          <span class="tool-label">Format</span>
          <SelectDropdown
            title="Mailbox interpretation"
            value={dialect}
            options={[
              { value: "mboxrd", label: "mboxrd" },
              { value: "mboxo", label: "mboxo" },
            ]}
            disabled={busy || Boolean(container)}
            onchange={(value) => {
              dialect = value;
              preview = null;
            }}
          />
        </div>
      </div>
      {#if error}<p role="alert" class="tool-error">{error}</p>{/if}
      {#if busy}
        <progress aria-label="Mailbox source upload" value={processed} max={total || 1}></progress>
      {/if}
      {#if !job && !(preview && container)}
        <div class="tool-actions">
          {#if busy}<span class="tool-muted" aria-live="polite"
              ><Spinner size={14} />
              {stage} · {formatBytes(processed)} of {formatBytes(total)}</span
            ><Button onclick={() => void cancel()}>Cancel upload</Button>{/if}
          <Button tone="info" surface="solid" disabled={busy || !file} onclick={() => void upload()}
            >{container ? "Refresh preview" : "Upload and preview"}</Button
          >
        </div>
      {/if}
    </section>
    {#if preview && container && !job}
      <section class="tool-section" aria-label="Import preview">
        <div class="tool-heading">
          <h3>Source verified</h3>
          <span class="tool-muted"
            >{preview.entry_count}
            {preview.entry_count === 1 ? "message" : "messages"} · {preview.dialect}</span
          >
        </div>
        {#if preview.samples.some((sample) => sample.rejection)}<p class="tool-error">
            Some messages exceed the import limits and will be reported as rejected.
          </p>{/if}
        <div class="tool-actions">
          <Button disabled={busy} onclick={() => void upload()}>Refresh preview</Button>
          <Button tone="info" surface="solid" disabled={busy} onclick={() => void start()}
            >Import messages</Button
          >
        </div>
      </section>
    {/if}
    {#if job}
      <section class="tool-section" aria-label="Import report">
        <div class="tool-heading">
          <h3>{job.state === "complete" ? "Import complete" : "Mailbox import"}</h3>
          <Chip
            size="xs"
            tone={job.state === "complete" ? "success" : job.state === "failed" ? "danger" : "info"}
            >{job.state}</Chip
          >
        </div>
        <dl class="tool-stats" aria-live="polite">
          <div>
            <dt>Imported</dt>
            <dd>{job.imported}</dd>
          </div>
          <div>
            <dt>Rejected</dt>
            <dd>{job.rejected}</dd>
          </div>
          <div>
            <dt>Pending</dt>
            <dd>{job.pending ?? 0}</dd>
          </div>
          <div>
            <dt>Canceled</dt>
            <dd>{job.canceled ?? 0}</dd>
          </div>
        </dl>
        {#if !job.scanned_tail}<p class="tool-muted">Unscanned messages remain.</p>{/if}
        {#if job.reason}<p>{job.reason}</p>{/if}
        <p class="tool-muted">Import {job.id}</p>
        <div class="tool-actions">
          {#if ["queued", "running"].includes(job.state)}<Button onclick={() => void cancel()}
              >Cancel import</Button
            >{/if}
          {#if job.state === "complete" && job.imported > 0 && onexport}<Button
              onclick={() => onexport?.(job!.collection_id, job!.imported)}
              >Export these messages</Button
            >{/if}
          {#if job.state === "partial" && !job.scanned_tail}<Button
              tone="info"
              surface="solid"
              onclick={() => void resume(true)}>Continue import</Button
            >{/if}
          {#if ["failed", "canceled"].includes(job.state) && !job.scanned_tail}<Button
              tone="info"
              surface="solid"
              onclick={() => void resume(false)}>Resume import</Button
            >{/if}
        </div>
      </section>
    {/if}
    {#if prior.length > 1}
      <section class="tool-section" aria-labelledby="mailbox-prior-heading">
        <h3 id="mailbox-prior-heading">Earlier imports</h3>
        <ul class="tool-list">
          {#each prior as item (item.id)}
            <li>
              <span class="tool-row-main"><span>{item.id}</span></span>
              <div class="tool-row-actions">
                <Chip size="xs" tone="neutral">{item.state}</Chip>
                <Button
                  size="sm"
                  disabled={job?.id === item.id}
                  onclick={() => {
                    job = item;
                  }}>Show</Button
                >
              </div>
            </li>
          {/each}
        </ul>
      </section>
    {/if}
  </div>
</DrawerShell>

<style>
  progress {
    width: 100%;
    height: 4px;
  }
</style>
