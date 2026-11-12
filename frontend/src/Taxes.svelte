<script lang="ts">
  import { onMount } from "svelte";
  import { Button, EmptyState, IconButton, Spinner, Table, TextInput } from "@kenn-io/kit-ui";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import DrawerShell from "./DrawerShell.svelte";
  import "./toolForm.css";
  import type { TaxRecord } from "./generated/docbank.js";
  import { TaxesView } from "./taxes.svelte.js";
  import { formatTaxAmount, taxStateLabel } from "./taxMoney.js";

  interface Props {
    session: string;
    onauthfailure: (cause: unknown) => void;
  }

  let { session, onauthfailure }: Props = $props();

  let view = $state.raw<TaxesView>();
  let yearText = $state("");
  let yearError = $state("");
  const filtered = $derived(!!view && (view.filters.filer !== "" || view.filters.year !== null));
  const filerName = (id: string) => view?.parties.get(id)?.display_name ?? "Unknown filer";

  onMount(() => {
    const taxes = new TaxesView(session, onauthfailure);
    view = taxes;
    void taxes.load();
    return () => taxes.dispose();
  });

  function selectFiler(filer: string) {
    if (view && view.filters.filer !== filer) void view.setFilters({ ...view.filters, filer });
  }

  function setYear(year: number | null) {
    yearError = "";
    yearText = year === null ? "" : String(year);
    if (view && view.filters.year !== year) void view.setFilters({ ...view.filters, year });
  }

  function applyYear() {
    const text = yearText.trim();
    if (text === "") return setYear(null);
    if (!/^\d{1,4}$/.test(text) || Number(text) < 1) {
      yearError = "Enter a year from 1 to 9999";
      return;
    }
    setYear(Number(text));
  }

  function clearFilters() {
    yearText = "";
    yearError = "";
    void view?.clear();
  }
</script>

{#snippet records(rows: TaxRecord[], label: string)}
  <Table unsorted ariaLabel={label} scroll="page" zebra={false}>
    {#snippet header()}
      <th scope="col">Title</th><th scope="col" class="year">Year</th><th scope="col" class="due"
        >Due</th
      ><th scope="col" class="state">State</th><th scope="col" class="amount"
        >Amount (minor units)</th
      >
    {/snippet}
    {#each rows as row (row.record_id)}
      {@const amount = formatTaxAmount(row.amount_minor, row.currency, row.direction)}
      <tr>
        <td class="title"
          ><span title={row.title}>{row.title}</span>{#if row.form_code}<span class="sub"
              >{row.form_code}</span
            >{/if}</td
        >
        <td>{row.tax_year ?? "—"}</td>
        <td>{row.due_date ?? "—"}</td>
        <td>{taxStateLabel(row.state)}</td>
        <td class="amount" title={amount.hint}
          ><span>{amount.text}</span>{#if amount.hint}<span class="kit-sr-only">{amount.hint}</span
            >{/if}<span class="sub">{amount.direction}</span></td
        >
      </tr>
    {/each}
  </Table>
{/snippet}

<DrawerShell title="Taxes" ariaLabel="Taxes" onclose={() => {}}>
  {#snippet actions()}
    <IconButton
      size="sm"
      ariaLabel="Refresh tax records"
      disabled={!view || view.loading}
      onclick={() => void view?.load()}
    >
      <RefreshCwIcon size="14" aria-hidden="true" />
    </IconButton>
  {/snippet}

  <div class="tool-body">
    <div class="taxes-layout">
      <nav class="rail" aria-label="Tax filters">
        <div class="filers">
          <span class="tool-label">Filer</span>
          <button
            type="button"
            class="nav-item"
            aria-current={view?.filters.filer === "" ? "page" : undefined}
            onclick={() => selectFiler("")}>All filers</button
          >
          {#each view?.filers ?? [] as party (party.party_id)}
            <button
              type="button"
              class="nav-item"
              aria-current={view?.filters.filer === party.party_id ? "page" : undefined}
              onclick={() => selectFiler(party.party_id)}
              >{party.display_name}{party.retired_at ? " · retired" : ""}</button
            >
          {/each}
          {#if view?.partyError}<p class="tool-error">{view.partyError}</p>{/if}
        </div>
        <div class="tool-field">
          <label class="tool-label" for="tax-year">Tax year</label>
          <TextInput
            id="tax-year"
            size="sm"
            inputmode="numeric"
            bind:value={yearText}
            invalid={!!yearError}
            onblur={applyYear}
            onkeydown={(event) => {
              if (event.key === "Enter") applyYear();
            }}
          />
          {#if yearError}<p class="tool-error">{yearError}</p>{/if}
          {#if view && view.filters.year !== null}
            <Button size="sm" onclick={() => setYear(null)}>All years</Button>
          {/if}
        </div>
        {#if filtered}<Button size="sm" onclick={clearFilters}>Clear filters</Button>{/if}
      </nav>

      <section class="records" aria-label="Tax records">
        {#if view?.error}
          <div class="tool-heading">
            <p class="tool-error" role="alert">{view.error}</p>
            <Button size="sm" onclick={() => void view?.retry()}>Try again</Button>
          </div>
        {/if}
        {#if !view || (view.loading && view.rows.length === 0)}
          <p class="tool-muted"><Spinner size={14} /> Loading…</p>
        {:else if view.rows.length === 0}
          {#if view.error}{:else if filtered}
            <EmptyState title="No records match these filters">
              <Button size="sm" onclick={clearFilters}>Clear filters</Button>
            </EmptyState>
          {:else}
            <EmptyState
              title="No tax records yet"
              description="Add records with docbank tax record add; they appear here."
            />
          {/if}
        {:else if view.filters.filer}
          {@render records(view.rows, "Tax records")}
        {:else}
          {#each view.groups as group (group.filerPartyID)}
            <div class="tool-section">
              <h3>{filerName(group.filerPartyID)}</h3>
              {@render records(group.rows, `Tax records for ${filerName(group.filerPartyID)}`)}
            </div>
          {/each}
        {/if}
        {#if view && view.nextOffset !== null && view.rows.length}
          <div>
            <Button size="sm" disabled={view.more} onclick={() => void view?.loadMore()}
              >{view.more ? "Loading…" : "Load more"}</Button
            >
          </div>
        {/if}
      </section>
    </div>
  </div>
</DrawerShell>

<style>
  .taxes-layout {
    display: grid;
    grid-template-columns: 220px minmax(0, 1fr);
    gap: var(--space-7);
    align-items: start;
  }

  .rail,
  .records,
  .filers {
    display: grid;
    align-content: start;
    gap: var(--space-6);
    min-width: 0;
  }

  .filers {
    gap: var(--space-2);
  }

  /* The header row's padding adds to this gap, so a filer heading needs a
     tighter one to sit closer to its own table than to the group above. */
  .tool-section {
    gap: var(--space-3);
  }

  td,
  th {
    font-variant-numeric: tabular-nums;
  }

  /* The app's table-head look, padded like kit-ui's body cells so header and
     cell edges line up. */
  th {
    padding: 6px 10px;
    border-bottom: 1px solid var(--border-muted);
    background: var(--bg-surface);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-medium);
    text-align: start;
    white-space: nowrap;
  }

  /* Each filer group is its own table. A fixed layout takes the header widths
     below as given, so every group shares the same column edges and Title
     takes the rest; the minimum keeps Title readable before narrow screens
     scroll the table sideways. */
  .records :global(.kit-table) {
    min-width: 46rem;
    table-layout: fixed;
  }

  .year {
    width: 4.5rem;
  }

  .due {
    width: 7rem;
  }

  .state {
    width: 11rem;
  }

  th.amount {
    width: 12rem;
  }

  td:not(.title) {
    white-space: nowrap;
  }

  .title > span:first-child {
    display: block;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .sub {
    display: block;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .amount {
    text-align: end;
    white-space: nowrap;
  }

  @media (max-width: 900px) {
    .taxes-layout {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
