<script lang="ts">
  import { onDestroy } from "svelte";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import XIcon from "@lucide/svelte/icons/x";
  import { Button, Card, Chip, CopyButton, DetailDrawer, EmptyState, IconButton, SelectDropdown, Spinner, TextInput } from "@kenn-io/kit-ui";
  import { APIError } from "./api-transport.js";
  import {
    batesRecipe,
    clearPendingBatesReservation,
    createBatesNamespace,
    downloadBatesExport,
    listBatesExports,
    listBatesNamespaces,
    listBatesSources,
    loadPendingBatesReservation,
    maxBatesPages,
    previewBatesRange,
    readBatesExport,
    reserveBatesRange,
    savePendingBatesReservation,
    startBatesExport,
    type BatesAllocation,
    type BatesExport,
    type BatesNamespace,
    type BatesPlan,
    type BatesPosition,
    type BatesSource,
    type PendingBatesReservation,
  } from "./bates.js";
  import { formatBytes, formatDate } from "./format.js";

  interface Props {
    session: string;
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onclose, onauthfailure }: Props = $props();
  let sources = $state<BatesSource[]>([]);
  let namespaces = $state<BatesNamespace[]>([]);
  let history = $state<BatesExport[]>([]);
  let sourceID = $state("");
  let namespaceID = $state("");
  let startAt = $state("0");
  let position = $state<BatesPosition>("bottom-right");
  let margin = $state("24");
  let prefix = $state("");
  let suffix = $state("");
  let padding = $state("6");
  let plan = $state<BatesPlan | null>(null);
  let pending = $state<PendingBatesReservation | null>(null);
  let restored = $state(false);
  let unsaved = $state(false);
  let allocation = $state<BatesAllocation | null>(null);
  let activeExport = $state<BatesExport | null>(null);
  let viewedExport = $state<BatesExport | null>(null);
  let loading = $state(true);
  let task = $state("");
  let error = $state("");
  let historyError = $state("");
  let controller = new AbortController();

  const busy = $derived(task !== "");
  const locked = $derived(busy || pending !== null);
  const eligible = (item: BatesSource): boolean => item.page_count <= maxBatesPages;
  const selectedSource = $derived(sources.find((item) => item.package_id === sourceID && eligible(item)) ?? sources.find(eligible));
  const selectedNamespace = $derived(namespaces.find((item) => item.namespace_id === namespaceID) ?? namespaces[0]);
  const sourceOptions = $derived(sources.map((item) => ({
    value: item.package_id,
    label: `${item.package_name} · ${item.page_count} ${item.page_count === 1 ? "page" : "pages"}${eligible(item) ? "" : ` · over the ${maxBatesPages}-page limit`}`,
    disabled: !eligible(item),
  })));
  const positionOptions = [
    { value: "top-left", label: "Top left" }, { value: "top-center", label: "Top center" }, { value: "top-right", label: "Top right" },
    { value: "bottom-left", label: "Bottom left" }, { value: "bottom-center", label: "Bottom center" }, { value: "bottom-right", label: "Bottom right" },
  ];
  const namespaceOptions = $derived(namespaces.map((item) => ({ value: item.namespace_id, label: `${item.prefix}${"0".repeat(item.padding)}${item.suffix}` })));

  $effect(() => {
    const current = session;
    controller.abort();
    controller = new AbortController();
    void load(current, controller.signal);
    return () => controller.abort();
  });
  onDestroy(() => controller.abort());

  function message(cause: unknown): string {
    return cause instanceof Error ? cause.message : String(cause);
  }

  function aborted(cause: unknown): boolean {
    return cause instanceof DOMException && cause.name === "AbortError";
  }

  function unauthorized(cause: unknown): boolean {
    if (!(cause instanceof APIError && cause.status === 401)) return false;
    onauthfailure(cause);
    onclose();
    return true;
  }

  function fail(cause: unknown): void {
    if (aborted(cause) || unauthorized(cause)) return;
    error = message(cause);
  }

  async function load(current: string, signal: AbortSignal): Promise<void> {
    loading = true;
    error = "";
    historyError = "";
    try {
      const [nextSources, nextNamespaces] = await Promise.all([
        listBatesSources(current, signal),
        listBatesNamespaces(current, signal),
      ]);
      if (signal.aborted || current !== session) return;
      sources = nextSources;
      namespaces = nextNamespaces;
      sourceID = nextSources.find(eligible)?.package_id ?? "";
      namespaceID = nextNamespaces[0]?.namespace_id ?? "";
      restore();
      try {
        const page = await listBatesExports(current, signal);
        if (!signal.aborted && current === session) history = page.items;
      } catch (cause) {
        if (!signal.aborted && current === session && !aborted(cause) && !unauthorized(cause)) historyError = message(cause);
      }
    } catch (cause) {
      if (!signal.aborted && current === session) fail(cause);
    } finally {
      if (!signal.aborted && current === session) loading = false;
    }
  }

  function restore(): void {
    const saved = loadPendingBatesReservation();
    if (!saved) return;
    pending = saved;
    restored = true;
    plan = saved.plan;
    sourceID = sources.find((item) => item.snapshot_id === saved.snapshot_id)?.package_id ?? sourceID;
    namespaceID = saved.plan.namespace.namespace_id;
    startAt = String(saved.recipe.start_at);
    position = saved.recipe.position as BatesPosition;
    margin = String(saved.recipe.margin_points);
  }

  function remember(next: PendingBatesReservation): void {
    pending = next;
    unsaved = !savePendingBatesReservation(next);
  }

  function resetReview(): void {
    plan = null;
    allocation = null;
    activeExport = null;
    error = "";
  }

  function discard(): void {
    clearPendingBatesReservation();
    pending = null;
    restored = false;
    unsaved = false;
    resetReview();
  }

  async function run(label: string, work: () => Promise<void>): Promise<void> {
    if (busy) return;
    task = label;
    error = "";
    try { await work(); } catch (cause) { fail(cause); } finally { task = ""; }
  }

  function createNamespace(): Promise<void> {
    return run("Creating namespace…", async () => {
      const created = await createBatesNamespace(session, prefix, suffix, Number(padding), controller.signal);
      namespaces = [...namespaces.filter((item) => item.namespace_id !== created.namespace_id), created];
      namespaceID = created.namespace_id;
      resetReview();
    });
  }

  function preview(): Promise<void> {
    const source = selectedSource;
    const namespace = selectedNamespace;
    if (!source || !namespace || pending) return Promise.resolve();
    return run("Checking selected pages…", async () => {
      resetReview();
      plan = await previewBatesRange(session, source.snapshot_id, namespace.namespace_id, Number(startAt), controller.signal);
    });
  }

  async function reserveWork(): Promise<void> {
    if (!pending) {
      if (!selectedSource || !plan) return;
      remember({
        operation_id: crypto.randomUUID(),
        snapshot_id: selectedSource.snapshot_id,
        recipe: batesRecipe(plan.namespace, plan.start_sequence, position, Number(margin)),
        plan,
      });
    }
    const current = pending;
    if (!current) return;
    task = "Reserving range…";
    allocation = await reserveBatesRange(session, current, controller.signal);
    remember({ ...current, allocation_id: allocation.allocation_id });
  }

  async function startWork(): Promise<void> {
    if (!allocation || !pending) return;
    task = "Stamping and verifying…";
    const published = await startBatesExport(session, allocation, pending.recipe, controller.signal);
    activeExport = published;
    history = [published, ...history.filter((item) => item.allocation_id !== published.allocation_id)];
    historyError = "";
    clearPendingBatesReservation();
    pending = null;
    restored = false;
    unsaved = false;
  }

  function reserve(): Promise<void> {
    return run("Reserving range…", reserveWork);
  }

  function start(): Promise<void> {
    return run("Stamping and verifying…", startWork);
  }

  function resume(): Promise<void> {
    return run("Reserving range…", async () => {
      await reserveWork();
      await startWork();
    });
  }

  function refreshExport(value: BatesExport): Promise<void> {
    return run("Refreshing receipt…", async () => {
      const next = await readBatesExport(session, value.allocation_id, controller.signal);
      if (activeExport?.allocation_id === next.allocation_id) activeExport = next;
      if (viewedExport?.allocation_id === next.allocation_id) viewedExport = next;
      history = [next, ...history.filter((item) => item.allocation_id !== next.allocation_id)];
      historyError = "";
    });
  }

  function download(value: BatesExport): Promise<void> {
    return run("Preparing download…", () => downloadBatesExport(session, value, controller.signal));
  }

  function view(item: BatesExport): void {
    if (busy) return;
    viewedExport = item.allocation_id === activeExport?.allocation_id ? null : item;
  }
</script>

{#snippet exportCard(item: BatesExport, title: string, eyebrow: string, onhide?: () => void)}
  <Card level="default" padding="sm" {title} {eyebrow}>
    {#snippet actions()}<Chip size="xs" tone="success">verified</Chip>{/snippet}
    <dl><div><dt>Pages</dt><dd>{item.page_count}</dd></div><div><dt>Published</dt><dd>{formatDate(item.created_at)}</dd></div><div><dt>PDF size</dt><dd>{formatBytes(item.size)}</dd></div></dl>
    <div class="identity"><span>PDF SHA-256</span><code>{item.blob_sha256}</code><CopyButton text={item.blob_sha256} ariaLabel="Copy Bates PDF hash" /></div>
    <div class="actions">
      <Button size="sm" disabled={busy} onclick={() => void refreshExport(item)}><RefreshCwIcon size="14" aria-hidden="true" /> Refresh receipt</Button>
      {#if onhide}<Button size="sm" disabled={busy} onclick={onhide}>Close</Button>{/if}
      <Button tone="info" disabled={busy} onclick={() => void download(item)}>Download verified PDF</Button>
    </div>
    <p>Status refresh is explicit. Closing the drawer does not cancel server work.</p>
  </Card>
{/snippet}

<DetailDrawer width="min(860px, 100vw)" ariaLabel="Bates export" onclose={onclose}>
  {#snippet header()}
    <div class="drawer-heading">
      <div><span>BATES EXPORT</span><strong>Review, reserve, and publish</strong><small>Sealed selected pages · durable numbering ledger</small></div>
      <IconButton size="sm" ariaLabel="Close Bates export" onclick={onclose}><XIcon size="14" aria-hidden="true" /></IconButton>
    </div>
  {/snippet}

  <div class="content">
    {#if loading}<div class="loading" role="status"><Spinner size={16} /> Loading Bates sources and namespaces…</div>{/if}
    {#if task}<div class="loading" role="status"><Spinner size={16} /> {task}</div>{/if}
    {#if error}<p class="error" role="alert">{error}</p>{/if}

    {#if !loading && sources.length === 0}
      <EmptyState title="No eligible page package" description="Import a reviewed package with a sealed selected PDF before creating a Bates export." />
    {:else if sources.length > 0}
      <section class="settings" aria-labelledby="bates-source-heading">
        <div class="section-heading"><div><span>SOURCE</span><strong id="bates-source-heading">Sealed selected pages</strong></div><Chip size="xs" tone="neutral">{selectedSource?.page_count ?? 0} pages</Chip></div>
        <SelectDropdown title="Bates source package" value={selectedSource?.package_id ?? ""} options={sourceOptions} disabled={locked} onchange={(value) => { sourceID = value; resetReview(); }} />
        <p>The reservation follows the package’s immutable selected-page order. Live document changes cannot alter it.</p>
        {#if sources.some((item) => !eligible(item))}<p class="muted">Packages over {maxBatesPages} pages cannot be stamped in one Bates export.</p>{/if}
      </section>

      <section class="settings" aria-labelledby="bates-namespace-heading">
        <div class="section-heading"><div><span>NUMBERING</span><strong id="bates-namespace-heading">Namespace and stamp</strong></div></div>
        {#if namespaces.length > 0}
          <SelectDropdown title="Bates namespace" value={selectedNamespace?.namespace_id ?? ""} options={namespaceOptions} disabled={locked} onchange={(value) => { namespaceID = value; resetReview(); }} />
        {:else}<p>Create the first namespace. Prefix and suffix identities cannot later change padding.</p>{/if}
        <div class="fields">
          <label for="bates-prefix">Prefix<TextInput id="bates-prefix" ariaLabel="Bates prefix" bind:value={prefix} disabled={locked} block /></label>
          <label for="bates-suffix">Suffix<TextInput id="bates-suffix" ariaLabel="Bates suffix" bind:value={suffix} disabled={locked} block /></label>
          <label>Padding<SelectDropdown title="Bates padding" value={padding} options={Array.from({ length: 10 }, (_, index) => ({ value: String(index + 1), label: `${index + 1} digits` }))} disabled={locked} onchange={(value) => padding = value} /></label>
        </div>
        <Button size="sm" disabled={locked} onclick={() => void createNamespace()}>{task === "Creating namespace…" ? task : "Create namespace"}</Button>

        {#if selectedNamespace}
          <div class="fields">
            <label for="bates-start">First number<TextInput id="bates-start" ariaLabel="First Bates number" bind:value={startAt} oninput={() => resetReview()} disabled={locked} block /></label>
            <label>Position<SelectDropdown title="Bates position" value={position} options={positionOptions} disabled={locked} onchange={(value) => { position = value as BatesPosition; resetReview(); }} /></label>
            <label for="bates-margin">Margin points<TextInput id="bates-margin" ariaLabel="Bates margin points" bind:value={margin} oninput={() => resetReview()} disabled={locked} block /></label>
          </div>
          <p>Use 0 for the next available number. Preview does not reserve the range.</p>
          <Button tone="info" disabled={locked || !selectedSource} onclick={() => void preview()}>{task === "Checking selected pages…" ? task : "Preview Bates labels"}</Button>
        {/if}
      </section>
    {/if}

    {#if plan}
      <section class="preview" aria-label="Tentative Bates labels">
        <div class="section-heading"><div><span>{allocation ? "REVIEWED" : "TENTATIVE"}</span><strong>{plan.labels[0]?.label}–{plan.labels.at(-1)?.label}</strong></div><Chip size="xs" tone={allocation ? "neutral" : "warning"}>{allocation ? "Reserved below" : pending ? "Reservation pending" : "Not reserved"}</Chip></div>
        <p>
          {#if activeExport}This reviewed preview produced the verified publication below.
          {:else if allocation}This reviewed preview is the exact range reserved below. Nothing has been stamped yet.
          {:else if restored}This tab reviewed this range earlier but did not finish the export. Resume to reserve the same range and stamp it.
          {:else if pending}The reservation did not complete. Retry sends the same request, so it cannot reserve a second range.
          {:else}Nothing has been stamped or reserved.{/if}
        </p>
        <div class="label-list">
          {#each plan.labels as label (label.ordinal)}
            <div><code>{label.label}</code><span>Source page {label.source_page}</span><span>Output page {label.output_page}</span></div>
          {/each}
        </div>
        {#if unsaved}<p class="muted">This browser could not remember the reservation. Keep this drawer open until the export finishes.</p>{/if}
        <div class="actions">
          {#if !pending}<Button tone="info" disabled={busy || Boolean(activeExport)} onclick={() => void reserve()}>Reserve {plan.labels[0]?.label}–{plan.labels.at(-1)?.label}</Button>
          {:else if !allocation && restored}<Button tone="info" disabled={busy} onclick={() => void resume()}>Resume Bates export</Button>
          {:else if !allocation}<Button tone="info" disabled={busy} onclick={() => void reserve()}>Retry reserve</Button>{/if}
          {#if pending}<Button disabled={busy} onclick={discard}>Discard review</Button>{/if}
        </div>
        {#if pending && allocation}<p class="muted">Discarding only forgets this review in this browser. The reserved numbers stay reserved and are never reused.</p>{/if}
      </section>
    {/if}

    {#if allocation}
      <Card level="default" padding="sm" title="Range reserved" eyebrow="DURABLE LEDGER">
        {#snippet actions()}<Chip size="xs" tone="success">reserved</Chip>{/snippet}
        <dl><div><dt>Labels</dt><dd>{allocation.labels[0]?.label}–{allocation.labels.at(-1)?.label}</dd></div><div><dt>Pages</dt><dd>{allocation.labels.length}</dd></div></dl>
        <div class="identity"><span>RECIPE SHA-256</span><code>{allocation.recipe_sha256}</code><CopyButton text={allocation.recipe_sha256} ariaLabel="Copy Bates recipe hash" /></div>
        {#if !activeExport}<Button tone="info" disabled={busy} onclick={() => void start()}>Start Bates export</Button>{/if}
      </Card>
    {/if}

    {#if activeExport}{@render exportCard(activeExport, "Stamped PDF ready", "VERIFIED PUBLICATION")}{/if}
    {#if viewedExport}{@render exportCard(viewedExport, "Earlier Bates export", "HISTORY", () => viewedExport = null)}{/if}

    <section class="history" aria-labelledby="bates-history-heading">
      <div class="section-heading"><div><span>HISTORY</span><strong id="bates-history-heading">Recent Bates exports</strong></div></div>
      {#if historyError}<p class="error" role="alert">Publication history is unavailable: {historyError}</p>
      {:else if history.length === 0}<p class="muted">No Bates exports have been published.</p>
      {:else}<fieldset class="history-list" disabled={busy}>{#each history as item (item.allocation_id)}<Card level="default" padding="sm" ariaLabel={`Open Bates export ${item.allocation_id}`} selected={viewedExport?.allocation_id === item.allocation_id} onclick={() => view(item)}><div class="history-row"><span>{formatDate(item.created_at)}</span><strong>{item.page_count} pages</strong><Chip size="xs" tone="success">{item.state}</Chip></div></Card>{/each}</fieldset>{/if}
    </section>
  </div>
</DetailDrawer>

<style>
  .drawer-heading,.section-heading,.actions,.identity{display:flex;align-items:center;justify-content:space-between;gap:var(--space-3)}
  .drawer-heading{width:100%}.drawer-heading>div,.section-heading>div,.content,.settings,.preview,.history{display:grid;gap:var(--space-3)}
  .drawer-heading span,.section-heading span,.identity>span{font-size:var(--font-size-xs);font-weight:var(--font-weight-bold);color:var(--text-muted)}
  .drawer-heading strong{font-size:var(--font-size-lg)}.drawer-heading small,.muted{color:var(--text-muted)}
  .content{padding:var(--space-5);gap:var(--space-5);overflow:auto}.settings,.preview,.history{padding:var(--space-4);border:1px solid var(--border-muted);border-radius:var(--radius-lg);background:var(--bg-inset)}
  .fields{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:var(--space-3)}label{display:grid;gap:var(--space-2);font-size:var(--font-size-sm)}
  p{margin:0;font-size:var(--font-size-sm);color:var(--text-secondary);line-height:1.5}.error{color:var(--accent-red)}
  .label-list{display:grid;max-height:240px;overflow:auto;border:1px solid var(--border-muted);border-radius:var(--radius-md)}.label-list>div{display:grid;grid-template-columns:minmax(150px,1fr) 1fr 1fr;gap:var(--space-3);padding:var(--space-2) var(--space-3);border-bottom:1px solid var(--border-muted);font-size:var(--font-size-sm)}.label-list>div:last-child{border-bottom:0}
  code{font-size:var(--font-size-xs);overflow-wrap:anywhere}.identity{justify-content:flex-start;flex-wrap:wrap}.identity>span{width:100%}
  dl{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:var(--space-3);margin:0}dt{font-size:var(--font-size-xs);color:var(--text-muted)}dd{margin:var(--space-1) 0 0;font-size:var(--font-size-sm)}
  .history-list{display:grid;gap:var(--space-2);min-width:0;margin:0;padding:0;border:0}.history-row{display:grid;grid-template-columns:1fr auto auto;align-items:center;gap:var(--space-3);color:var(--text-primary);text-align:left}.history-list span{font-size:var(--font-size-sm);color:var(--text-secondary)}
  .loading{display:flex;align-items:center;gap:var(--space-2)}
  @media(max-width:640px){.fields,.label-list>div,dl{grid-template-columns:1fr}.actions{align-items:stretch;flex-direction:column}}
</style>
