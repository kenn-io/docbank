<script lang="ts">
  import { listPackages, readBatesAllocation, type PackageSummary } from "./generated/docbank.js";
  import InspectorDetails from "./InspectorDetails.svelte";
  import DrawerShell from "./DrawerShell.svelte";
  import { onDestroy } from "svelte";
  import ChevronRightIcon from "@lucide/svelte/icons/chevron-right";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import { Button, Chip, CopyButton, SelectDropdown, Spinner, TextInput } from "@kenn-io/kit-ui";
  import "./toolForm.css";
  import { APIError } from "./api-transport.js";
  import { randomUUID } from "./crypto.js";
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
    mode?: "create" | "productions";
    onclose: () => void;
    onauthfailure: (cause: unknown) => void;
    oncomplete?: () => void;
  }

  let { session, mode = "create", onclose, onauthfailure, oncomplete }: Props = $props();
  let packages = $state<PackageSummary[]>([]);
  let history = $state<BatesExport[]>([]);
  let exportSnapshots = $state<Record<string, string>>({});
  const productions = $derived([
    ...history.map((receipt) => ({
      id: receipt.allocation_id,
      receipt,
      package: packages.find(
        (item) => item.snapshot_id && item.snapshot_id === exportSnapshots[receipt.allocation_id],
      ),
      range: `${receipt.pages[0]?.label}–${receipt.pages.at(-1)?.label}`,
      pages: receipt.page_count,
    })),
    ...packages
      .filter(
        (item) =>
          !history.some(
            (receipt) =>
              item.snapshot_id && item.snapshot_id === exportSnapshots[receipt.allocation_id],
          ),
      )
      .map((item) => ({
        id: item.package_id,
        package: item,
        receipt: null,
        range: "",
        pages: item.page_count,
      })),
  ]);
  let sources = $state<BatesSource[]>([]);
  let namespaces = $state<BatesNamespace[]>([]);
  let sourceID = $state("");
  let namespaceID = $state("");
  let startAt = $state("");
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
  const selectedSource = $derived(
    sources.find((item) => item.package_id === sourceID && eligible(item)) ??
      sources.find(eligible),
  );
  const selectedNamespace = $derived(
    namespaces.find((item) => item.namespace_id === namespaceID) ?? namespaces[0],
  );
  const sourceOptions = $derived(
    sources.map((item) => ({
      value: item.package_id,
      label: `${item.package_name} · ${item.page_count} ${item.page_count === 1 ? "page" : "pages"}${eligible(item) ? "" : ` · over the ${maxBatesPages}-page limit`}`,
      disabled: !eligible(item),
    })),
  );
  const positionOptions = [
    { value: "top-left", label: "Top left" },
    { value: "top-center", label: "Top center" },
    { value: "top-right", label: "Top right" },
    { value: "bottom-left", label: "Bottom left" },
    { value: "bottom-center", label: "Bottom center" },
    { value: "bottom-right", label: "Bottom right" },
  ];
  const namespaceOptions = $derived(
    namespaces.map((item) => ({
      value: item.namespace_id,
      label: `${item.prefix}${"0".repeat(item.padding)}${item.suffix}`,
    })),
  );

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

  async function loadPackages(current: string, signal: AbortSignal): Promise<void> {
    const items: PackageSummary[] = [];
    let after: string | undefined;
    do {
      const page = await listPackages(
        { limit: 250, ...(after ? { after } : {}) },
        { session: current, signal },
      );
      if (signal.aborted || current !== session) return;
      items.push(...page.items);
      packages = [...items];
      if (page.next_after && page.next_after === after)
        throw new Error("The package list did not advance.");
      after = page.next_after;
    } while (after);
  }

  async function loadHistory(current: string, signal: AbortSignal): Promise<void> {
    const items: BatesExport[] = [];
    let after: string | undefined;
    do {
      const page = await listBatesExports(current, signal, after);
      if (signal.aborted || current !== session) return;
      items.push(...page.items);
      history = [...items];
      if (page.next_after && page.next_after === after)
        throw new Error("The export list did not advance.");
      after = mode === "productions" ? page.next_after : undefined;
    } while (after);
    if (mode !== "productions") return;
    await Promise.all(
      items.map(async (item) => {
        try {
          const allocation = await readBatesAllocation(item.allocation_id, {
            session: current,
            signal,
          });
          if (signal.aborted || current !== session) return;
          exportSnapshots[item.allocation_id] = allocation.snapshot_id;
        } catch (cause) {
          if (signal.aborted || current !== session || aborted(cause) || unauthorized(cause))
            return;
          historyError = message(cause);
        }
      }),
    );
  }

  async function load(current: string, signal: AbortSignal): Promise<void> {
    loading = true;
    error = "";
    historyError = "";
    try {
      try {
        if (mode === "productions") {
          await loadPackages(current, signal);
        } else {
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
        }
      } catch (cause) {
        if (signal.aborted || current !== session || aborted(cause)) return;
        if (unauthorized(cause)) return;
        error = message(cause);
      }
      try {
        await loadHistory(current, signal);
      } catch (cause) {
        if (!signal.aborted && current === session && !aborted(cause) && !unauthorized(cause))
          historyError = message(cause);
      }
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
    sourceID =
      sources.find((item) => item.snapshot_id === saved.snapshot_id)?.package_id ?? sourceID;
    namespaceID = saved.plan.namespace.namespace_id;
    startAt = saved.recipe.start_at ? String(saved.recipe.start_at) : "";
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
    try {
      await work();
    } catch (cause) {
      fail(cause);
    } finally {
      task = "";
    }
  }

  function createNamespace(): Promise<void> {
    return run("Creating namespace…", async () => {
      const created = await createBatesNamespace(
        session,
        prefix,
        suffix,
        Number(padding),
        controller.signal,
      );
      namespaces = [
        ...namespaces.filter((item) => item.namespace_id !== created.namespace_id),
        created,
      ];
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
      plan = await previewBatesRange(
        session,
        source.snapshot_id,
        namespace.namespace_id,
        Number(startAt),
        controller.signal,
      );
    });
  }

  async function reserveWork(): Promise<void> {
    if (!pending) {
      if (!selectedSource || !plan) return;
      remember({
        operation_id: randomUUID(),
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
    const published = await startBatesExport(
      session,
      allocation,
      pending.recipe,
      controller.signal,
    );
    activeExport = published;
    oncomplete?.();
    history = [
      published,
      ...history.filter((item) => item.allocation_id !== published.allocation_id),
    ];
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

{#snippet exportCard(item: BatesExport, title: string, onhide?: () => void)}
  <section class="tool-section" aria-label={title}>
    {#if title}<div class="tool-heading">
        <h3>{title}</h3>
        <Chip size="xs" tone="success">verified</Chip>
      </div>{/if}
    <dl class="tool-stats">
      <div>
        <dt>Pages</dt>
        <dd>{item.page_count}</dd>
      </div>
      <div>
        <dt>Published</dt>
        <dd>{formatDate(item.created_at)}</dd>
      </div>
      <div>
        <dt>PDF size</dt>
        <dd>{formatBytes(item.size)}</dd>
      </div>
    </dl>
    <dl class="hash">
      <div>
        <dt>PDF SHA-256</dt>
        <dd>
          <code>{item.blob_sha256}</code><CopyButton
            text={item.blob_sha256}
            ariaLabel="Copy Bates PDF hash"
          />
        </dd>
      </div>
    </dl>
    <div class="tool-actions">
      {#if onhide}<Button disabled={busy} onclick={onhide}>Close</Button>{/if}
      <Button disabled={busy} onclick={() => void refreshExport(item)}
        ><RefreshCwIcon size="14" aria-hidden="true" /> Refresh receipt</Button
      >
      <Button tone="info" surface="solid" disabled={busy} onclick={() => void download(item)}
        >Download verified PDF</Button
      >
    </div>
  </section>
{/snippet}

<DrawerShell
  title={mode === "productions" ? "Productions" : "Bates-numbered PDFs"}
  closeLabel={mode === "productions" ? "Close Productions" : "Close Bates export"}
  {onclose}
>
  <div class="tool-body">
    {#if loading}<p class="tool-muted" role="status"><Spinner size={14} /> Loading…</p>{/if}
    {#if task}<p class="tool-muted" role="status"><Spinner size={14} /> {task}</p>{/if}
    {#if error}<p class="tool-error" role="alert">{error}</p>{/if}

    {#if mode === "productions"}
      {#if !loading && !error && !historyError && !packages.length && !history.length}
        <p class="tool-muted">No productions yet</p>
      {/if}
      {#if productions.length}
        <ul class="tool-list" aria-label="Productions">
          {#each productions as item (item.id)}
            <li class="production-item">
              <details class="tool-disclosure">
                <summary>
                  <ChevronRightIcon size="14" aria-hidden="true" />
                  <span class="production-name"
                    >{item.package?.package_name ?? "Bates export"}<span class="production-range"
                      >{item.range || "—"}</span
                    ></span
                  >
                  <span class="tool-muted">{item.pages} {item.pages === 1 ? "page" : "pages"}</span>
                  <span
                    >{item.receipt || item.package?.direction === "produced"
                      ? "Exported"
                      : "Imported"}</span
                  >
                </summary>
                <div class="production-details">
                  {#if item.package}
                    <InspectorDetails
                      facts={[
                        { label: "Documents", value: item.package.member_count },
                        { label: "Party", value: item.package.party_label },
                        { label: "Created", value: formatDate(item.package.created_at) },
                        { label: "Fingerprint", value: item.package.manifest_sha256, copy: true },
                      ]}
                    />
                  {/if}
                  {#if item.receipt}{@render exportCard(item.receipt, "")}{/if}
                </div>
              </details>
            </li>
          {/each}
        </ul>
      {/if}
      {#if historyError}<p class="tool-error" role="alert">
          Publication history is unavailable: {historyError}
        </p>{/if}
    {:else}
      {#if !loading && sources.length === 0}
        <p class="tool-muted">
          No PDF packages yet. Import a load-file package with PDF pages from Add → Import load
          files.
        </p>
      {:else if sources.length > 0}
        <section class="tool-section" aria-label="Bates settings">
          <div class="tool-grid">
            <div
              class="tool-field"
              class:tool-span-3={namespaces.length > 0}
              class:tool-span-all={namespaces.length === 0}
            >
              <span class="tool-label">Package</span>
              <SelectDropdown
                title="Bates source package"
                value={selectedSource?.package_id ?? ""}
                options={sourceOptions}
                disabled={locked}
                onchange={(value) => {
                  sourceID = value;
                  resetReview();
                }}
              />
            </div>
            {#if namespaces.length > 0}
              <div class="tool-field tool-span-3">
                <span class="tool-label">Namespace</span>
                <SelectDropdown
                  title="Bates namespace"
                  value={selectedNamespace?.namespace_id ?? ""}
                  options={namespaceOptions}
                  disabled={locked}
                  onchange={(value) => {
                    namespaceID = value;
                    resetReview();
                  }}
                />
              </div>
            {/if}
            {#if selectedNamespace}
              <div class="tool-field tool-span-2">
                <label for="bates-start">First number</label>
                <TextInput
                  id="bates-start"
                  ariaLabel="First Bates number"
                  bind:value={startAt}
                  placeholder="Next available"
                  oninput={() => resetReview()}
                  disabled={locked}
                  block
                />
              </div>
              <div class="tool-field tool-span-2">
                <span class="tool-label">Position</span>
                <SelectDropdown
                  title="Bates position"
                  value={position}
                  options={positionOptions}
                  disabled={locked}
                  onchange={(value) => {
                    position = value as BatesPosition;
                    resetReview();
                  }}
                />
              </div>
              <div class="tool-field tool-span-2">
                <label for="bates-margin">Margin, points</label>
                <TextInput
                  id="bates-margin"
                  ariaLabel="Bates margin points"
                  bind:value={margin}
                  oninput={() => resetReview()}
                  disabled={locked}
                  block
                />
              </div>
            {/if}
          </div>
        </section>

        {#snippet namespaceForm()}
          <div class="tool-grid namespace-form">
            <div class="tool-field tool-span-2">
              <label for="bates-prefix">Prefix</label>
              <TextInput
                id="bates-prefix"
                ariaLabel="Bates prefix"
                bind:value={prefix}
                disabled={locked}
                block
              />
            </div>
            <div class="tool-field tool-span-2">
              <label for="bates-suffix">Suffix</label>
              <TextInput
                id="bates-suffix"
                ariaLabel="Bates suffix"
                bind:value={suffix}
                disabled={locked}
                block
              />
            </div>
            <div class="tool-field tool-span-2">
              <span class="tool-label">Padding</span>
              <SelectDropdown
                title="Bates padding"
                value={padding}
                options={Array.from({ length: 10 }, (_, index) => ({
                  value: String(index + 1),
                  label: `${index + 1} digits`,
                }))}
                disabled={locked}
                onchange={(value) => (padding = value)}
              />
            </div>
          </div>
          <div class="tool-actions">
            <Button
              tone={namespaces.length ? "neutral" : "info"}
              surface={namespaces.length ? "outline" : "solid"}
              disabled={locked}
              onclick={() => void createNamespace()}
              >{task === "Creating namespace…" ? task : "Create namespace"}</Button
            >
          </div>
        {/snippet}

        {#if namespaces.length === 0}
          <section class="tool-section" aria-labelledby="bates-namespace-heading">
            <h3 id="bates-namespace-heading">New namespace</h3>
            {@render namespaceForm()}
          </section>
        {:else}
          <details class="tool-disclosure">
            <summary
              ><ChevronRightIcon size="14" aria-hidden="true" /><span class="tool-label"
                >New namespace</span
              ></summary
            >
            <div class="tool-section">{@render namespaceForm()}</div>
          </details>
          {#if !plan}
            <div class="tool-actions">
              <Button
                tone="info"
                surface="solid"
                disabled={locked || !selectedSource}
                onclick={() => void preview()}
                >{task === "Checking selected pages…" ? task : "Preview Bates labels"}</Button
              >
            </div>
          {/if}
        {/if}
      {/if}

      {#if plan}
        <section class="tool-section" aria-label="Tentative Bates labels">
          <div class="tool-heading">
            <h3>{plan.labels[0]?.label}–{plan.labels.at(-1)?.label}</h3>
            <Chip size="xs" tone={allocation ? "neutral" : "warning"}
              >{allocation ? "Reserved" : pending ? "Reservation pending" : "Not reserved"}</Chip
            >
          </div>
          {#if !activeExport && allocation}<p class="tool-muted">
              Reserved below. Not stamped yet.
            </p>
          {:else if !activeExport && restored}<p class="tool-muted">
              This tab didn't finish this export. Resume reserves the same range and stamps it.
            </p>
          {:else if !activeExport && pending}<p class="tool-muted">
              The reservation didn't finish. Retry sends the same request, so it can't reserve a
              second range.
            </p>{/if}
          <div class="label-list" role="table" aria-label="Labels">
            <div class="label-row label-head" role="row">
              <span role="columnheader">Label</span><span role="columnheader">Source page</span
              ><span role="columnheader">Output page</span>
            </div>
            {#each plan.labels as label (label.ordinal)}
              <div class="label-row" role="row">
                <span role="cell"><code>{label.label}</code></span><span role="cell"
                  ><span class="kit-sr-only">{"Source page "}</span>{label.source_page}</span
                ><span role="cell"
                  ><span class="kit-sr-only">{"Output page "}</span>{label.output_page}</span
                >
              </div>
            {/each}
          </div>
          {#if unsaved}<p class="tool-error">
              This browser can't remember the reservation. Keep this drawer open until the export
              finishes.
            </p>{/if}
          {#if pending && allocation}<p class="tool-muted">
              Discarding forgets this review here. Reserved numbers stay reserved and are never
              reused.
            </p>{/if}
          <div class="tool-actions">
            {#if pending}<Button disabled={busy} onclick={discard}>Discard review</Button>{/if}
            {#if !pending}<Button disabled={busy} onclick={resetReview}>Change settings</Button
              >{/if}
            {#if !pending && !activeExport}<Button
                tone="info"
                surface="solid"
                disabled={busy}
                onclick={() => void reserve()}
                >Reserve {plan.labels[0]?.label}–{plan.labels.at(-1)?.label}</Button
              >
            {:else if !allocation && restored}<Button
                tone="info"
                surface="solid"
                disabled={busy}
                onclick={() => void resume()}>Resume Bates export</Button
              >
            {:else if !allocation}<Button
                tone="info"
                surface="solid"
                disabled={busy}
                onclick={() => void reserve()}>Retry reserve</Button
              >{/if}
          </div>
        </section>
      {/if}

      {#if allocation}
        <section class="tool-section" aria-label="Reserved range">
          <div class="tool-heading">
            <h3>Range reserved</h3>
            <Chip size="xs" tone="success">reserved</Chip>
          </div>
          <dl class="tool-stats">
            <div>
              <dt>Labels</dt>
              <dd>{allocation.labels[0]?.label}–{allocation.labels.at(-1)?.label}</dd>
            </div>
            <div>
              <dt>Pages</dt>
              <dd>{allocation.labels.length}</dd>
            </div>
          </dl>
          <dl class="hash">
            <div>
              <dt>Recipe SHA-256</dt>
              <dd>
                <code>{allocation.recipe_sha256}</code><CopyButton
                  text={allocation.recipe_sha256}
                  ariaLabel="Copy Bates recipe hash"
                />
              </dd>
            </div>
          </dl>
          {#if !activeExport}<div class="tool-actions">
              <Button tone="info" surface="solid" disabled={busy} onclick={() => void start()}
                >Start Bates export</Button
              >
            </div>{/if}
        </section>
      {/if}

      {#if activeExport}{@render exportCard(activeExport, "Stamped PDF ready")}{/if}
    {/if}
    {#if mode === "create" && viewedExport}{@render exportCard(
        viewedExport,
        "Earlier Bates export",
        () => (viewedExport = null),
      )}{/if}

    {#if mode === "create" && (history.length || historyError)}
      <section class="tool-section" aria-labelledby="bates-history-heading">
        <h3 id="bates-history-heading">Recent exports</h3>
        {#if historyError}<p class="tool-error" role="alert">
            Publication history is unavailable: {historyError}
          </p>
        {:else}<ul class="tool-list">
            {#each history as item (item.allocation_id)}<li class="history-item">
                <button
                  type="button"
                  class="history-row"
                  class:selected={viewedExport?.allocation_id === item.allocation_id}
                  aria-label={`Open Bates export ${item.allocation_id}`}
                  aria-pressed={viewedExport?.allocation_id === item.allocation_id}
                  disabled={busy}
                  onclick={() => view(item)}
                >
                  <span>{formatDate(item.created_at)}</span><span class="tool-muted"
                    >{item.page_count} pages</span
                  ><Chip size="xs" tone="success">{item.state}</Chip>
                </button>
              </li>{/each}
          </ul>{/if}
      </section>
    {/if}
  </div>
</DrawerShell>

<style>
  .tool-list > li.production-item {
    padding: 0;
    display: block;
  }
  .production-item .tool-disclosure {
    border: 0;
  }
  .production-item summary {
    gap: var(--space-4);
  }
  .production-name {
    flex: 1;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .production-range {
    display: block;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
  .production-details {
    display: grid;
    gap: var(--space-5);
  }
  .label-list {
    display: grid;
    max-height: 240px;
    overflow: auto;
    border: 1px solid var(--border-muted);
    border-radius: var(--radius-md);
  }
  .label-row {
    display: grid;
    grid-template-columns: minmax(150px, 1fr) 1fr 1fr;
    gap: var(--space-5);
    padding: var(--space-3) var(--space-5);
    border-bottom: 1px solid var(--border-muted);
    font-variant-numeric: tabular-nums;
  }
  .label-row:last-child {
    border-bottom: 0;
  }
  .label-head {
    position: sticky;
    top: 0;
    color: var(--text-secondary);
    font-size: var(--font-size-sm);
    font-weight: var(--font-weight-medium, 500);
    background: var(--bg-inset);
  }
  code {
    font-size: var(--font-size-sm);
    overflow-wrap: anywhere;
  }
  .hash {
    margin: 0;
  }
  .hash > div {
    grid-template-columns: minmax(0, 1fr);
    gap: var(--space-2);
  }
  .hash dd {
    display: flex;
    align-items: center;
    gap: var(--space-3);
  }
  .tool-list > li.history-item {
    padding: 0;
  }
  .history-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto auto;
    align-items: center;
    gap: var(--space-5);
    width: 100%;
    padding: var(--space-4) var(--space-3);
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-primary);
    font: inherit;
    text-align: left;
    cursor: pointer;
  }
  .history-row:hover:not(:disabled),
  .history-row.selected {
    background: var(--bg-inset);
  }
  .history-row:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: -2px;
  }
  @media (max-width: 640px) {
    .label-row {
      grid-template-columns: 1fr 1fr 1fr;
    }
  }
</style>
