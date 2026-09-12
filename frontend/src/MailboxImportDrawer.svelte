<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import XIcon from "@lucide/svelte/icons/x";
  import MailIcon from "@lucide/svelte/icons/mail";
  import {
    Button,
    Card,
    Chip,
    DetailDrawer,
    IconButton,
    SelectDropdown,
    Spinner,
  } from "@kenn-io/kit-ui";
  import { APIError, requestJSON, type Node } from "./api.js";
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
  }
  let { session, channel, directory, onclose, oncomplete, onauthfailure }: Props = $props();
  let file = $state<File | null>(null),
    dialect = $state("mboxrd"),
    busy = $state(false),
    error = $state(""),
    stage = $state("");
  let processed = $state(0),
    total = $state(0),
    containerID = $state("");
  let container = $state<MailboxContainer | null>(null),
    preview = $state<MailboxPreview | null>(null),
    job = $state<MailboxJob | null>(null),
    prior = $state<MailboxJob[]>([]);
  let controller: AbortController | null = null;
  const lifecycle = new AbortController();
  let timer: ReturnType<typeof setInterval> | undefined;
  let refreshing = false;
  onMount(() => {
    void refreshJobs();
    timer = setInterval(() => void refreshJobs(), 1000);
  });
  onDestroy(() => {
    controller?.abort();
    lifecycle.abort();
    if (timer) clearInterval(timer);
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
      prior = items;
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
      } else if (!file && items.length) job = items.at(-1) ?? null;
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
    job = null;
    error = "";
  }
  async function upload(): Promise<void> {
    if (!file || busy) return;
    busy = true;
    error = "";
    controller = new AbortController();
    if (!containerID) containerID = crypto.randomUUID();
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
      stage = "Checking archive and preview";
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
    try {
      job = await mailboxJSON<MailboxJob>(
        session,
        "/jobs",
        {
          id: container.id,
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
        await requestJSON(`/api/v1/mailbox/containers/${encodeURIComponent(containerID)}`, session, {
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

<DetailDrawer width="min(720px, 100vw)" ariaLabel="Import mailbox archive" {onclose}>
  {#snippet header()}
    <div class="drawer-heading"><div><span>EMAIL DOCUMENTS</span><strong>Import mailbox archive</strong><small>{directory.path??"Current folder"}</small></div><IconButton size="sm" ariaLabel="Close mailbox import" onclick={onclose}><XIcon size="14" /></IconButton></div>
  {/snippet}
  <div class="mailbox">
    <p>Keep the original MBOX or Google Takeout ZIP and import each message with its attachments.</p>
    <label class="file-label">Choose MBOX or Takeout ZIP<input type="file" accept=".mbox,.zip" aria-label="Choose MBOX or Takeout ZIP" disabled={busy} onchange={choose} /></label>
    <div class="choice"><span>Mailbox interpretation</span><SelectDropdown title="Mailbox interpretation" value={dialect} options={[{value:"mboxrd",label:"mboxrd (default)"},{value:"mboxo",label:"mboxo"}]} disabled={busy||Boolean(container)} onchange={(value)=>{dialect=value;preview=null;}} /></div>
    <p class="muted">The dialect is explicit. Source labels stay in provenance; importing does not authorize remote processing.</p>
    {#if file}<div class="file-name"><MailIcon size="18" /><strong>{file.name}</strong><span>{formatBytes(file.size)}</span></div>{/if}
    {#if file&&!job}<Button tone="info" disabled={busy} onclick={()=>void upload()}>{container?"Refresh preview":"Upload and preview"}</Button>{/if}
    {#if busy}<div class="progress" aria-live="polite"><Spinner size={16}/><span>{stage} · {formatBytes(processed)} / {formatBytes(total)}</span></div><progress aria-label="Mailbox source upload" value={processed} max={total||1}></progress><Button size="sm" onclick={()=>void cancel()}>Cancel upload</Button>{/if}
    {#if error}<p role="alert" class="error">{error}</p>{/if}
    {#if preview&&container}
      <Card title="Source verified" eyebrow="IMPORT PREVIEW" padding="md">
        <p>{preview.entry_count} mailbox {preview.entry_count===1?"entry":"entries"} · {preview.dialect}</p>
        <p>Previewed {preview.samples.length} {preview.has_more?"initial ":""}messages. Every occurrence is kept, including repeated content and Message-IDs.</p>
        {#if preview.samples.some(sample=>sample.rejection)}<p class="error">Some preview messages exceed the import limits and will be reported as rejected.</p>{/if}
        <Button tone="success" disabled={busy} onclick={()=>void start()}>Import messages</Button>
      </Card>
    {/if}
    {#if job}
      <Card title={job.state==="complete"?"Import complete":"Mailbox import"} eyebrow="DURABLE IMPORT REPORT" padding="md">
        {#snippet actions()}{#if job}<Chip size="xs" tone={job.state==="complete"?"success":job.state==="failed"?"danger":"info"}>{job.state}</Chip>{/if}{/snippet}
        <div class="counts" aria-live="polite"><div><strong>{job.imported}</strong><span>Imported</span></div><div><strong>{job.rejected}</strong><span>Rejected</span></div><div><strong>{job.retries}</strong><span>Retry receipts</span></div><div><strong>{job.pending??0}</strong><span>Pending</span></div><div><strong>{job.canceled??0}</strong><span>Canceled</span></div></div>
        <p>{job.scanned_tail?"Every message occurrence has been scanned.":"Unscanned messages remain."}</p>
        {#if job.reason}<p>{job.reason}</p>{/if}
        <p class="muted">Attachment documents are retained. Their indexing status depends on the configured processing profile and consent.</p>
        <small>Import {job.id}</small>
        <div class="actions">
          {#if ["queued","running"].includes(job.state)}<Button size="sm" onclick={()=>void cancel()}>Cancel import</Button>{/if}
          {#if job.state==="partial"&&!job.scanned_tail}<Button size="sm" onclick={()=>void resume(true)}>Continue import</Button>{/if}
          {#if ["failed","canceled"].includes(job.state)&&!job.scanned_tail}<Button size="sm" onclick={()=>void resume(false)}>Resume import</Button>{/if}
        </div>
      </Card>
    {/if}
    {#if prior.length>1}<div class="prior"><strong>Retained imports</strong>{#each prior as item(item.id)}<Button size="sm" onclick={()=>{job=item;}}>{item.id} · {item.state}</Button>{/each}</div>{/if}
  </div>
</DetailDrawer>
<style>
  .drawer-heading {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-4);
    width: 100%;
    min-width: 0;
  }
  .drawer-heading > div {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .drawer-heading span {
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
  .mailbox {
    display: grid;
    gap: 16px;
    padding: 24px;
    overflow: auto;
  }
  .mailbox p {
    margin: 0;
    line-height: 1.55;
  }
  .muted,
  small {
    color: var(--text-secondary);
    font-size: 12px;
  }
  .choice,
  .file-name,
  .progress,
  .actions {
    display: flex;
    align-items: center;
    gap: 12px;
    flex-wrap: wrap;
  }
  .choice {
    justify-content: space-between;
  }
  .file-label {
    display: grid;
    gap: 12px;
    font-size: 13px;
  }
  .file-name {
    padding: 12px;
    border: 1px solid var(--border);
    border-radius: 8px;
  }
  .file-name strong {
    flex: 1;
    overflow-wrap: anywhere;
  }
  .counts {
    display: flex;
    flex-wrap: wrap;
    gap: 24px;
    margin: 16px 0;
  }
  .counts div {
    display: grid;
    gap: 4px;
  }
  .counts strong {
    font-size: 26px;
  }
  .counts span {
    font-size: 12px;
    color: var(--text-secondary);
  }
  .error {
    color: var(--danger-text);
  }
  progress {
    width: 100%;
  }
  .actions {
    margin-top: 16px;
  }
  .prior {
    display: grid;
    gap: 8px;
  }
  .mailbox :global(.kit-card) {
    display: grid;
    gap: 12px;
  }
</style>
