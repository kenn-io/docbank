<script lang="ts">
  import { tick, untrack } from "svelte";
  import RefreshCwIcon from "@lucide/svelte/icons/refresh-cw";
  import { Button, Chip, IconButton, SelectDropdown, Spinner, Table } from "@kenn-io/kit-ui";
  import "./toolForm.css";
  import DrawerShell from "./DrawerShell.svelte";
  import ContractDetail from "./ContractDetail.svelte";
  import { APIError } from "./api-transport.js";
  import { listContracts, type Contract } from "./generated/docbank.js";
  import {
    addCivilDays,
    contractListParams,
    contractPageSize,
    contractStatusLabel,
    contractStatusOptions,
    contractTypeLabel,
    contractTypeOptions,
    counterpartyNames,
    expiryOptions,
    filtersSet,
    formatCivilDate,
    noContractFilters,
    statusTone,
    visibleContracts,
    type ContractFilters,
    type ExpiryWindow,
  } from "./contracts.js";

  let { session, onauthfailure }: { session: string; onauthfailure: (cause: unknown) => void } =
    $props();

  let filters = $state<ContractFilters>({ ...noContractFilters });
  let asOf = $state("");
  let items = $state<Contract[]>([]);
  let nextOffset = 0;
  let exhausted = $state(false);
  let loading = $state(true);
  let loadingMore = $state(false);
  let error = $state("");
  let moreError = $state("");
  let seenTypes = $state<string[]>([]);
  let selectedID = $state<string | null>(null);
  let detailRevision = $state(0);
  let generation = 0;
  let controller: AbortController | undefined;

  const rows = $derived(visibleContracts(items, filters, asOf));
  const typeOptions = $derived(contractTypeOptions([...seenTypes, filters.type]));

  $effect(() => {
    void session;
    untrack(() => void reload(true));
    return () => {
      generation += 1;
      controller?.abort();
    };
  });

  function message(cause: unknown): string {
    return cause instanceof Error ? cause.message : String(cause);
  }

  function failed(cause: unknown): string {
    if (cause instanceof APIError && cause.status === 401) {
      onauthfailure(cause);
      return "";
    }
    return message(cause);
  }

  function begin(): { request: number; signal: AbortSignal } {
    controller?.abort();
    controller = new AbortController();
    return { request: ++generation, signal: controller.signal };
  }

  // fetchPages appends full pages until a row is visible or the register
  // ends, all at one as_of. A horizon can hide a whole page of expired rows.
  async function fetchPages(request: number, signal: AbortSignal): Promise<boolean> {
    const shown = rows.length;
    while (true) {
      const page = await listContracts(contractListParams(filters, asOf, nextOffset), {
        session,
        signal,
      });
      if (request !== generation) return false;
      if (!asOf) asOf = page.as_of;
      nextOffset += page.items.length;
      items = [...items, ...page.items];
      seenTypes = [...new Set([...seenTypes, ...page.items.map((item) => item.contract_type)])];
      exhausted = page.items.length < contractPageSize;
      if (exhausted || rows.length > shown) return true;
    }
  }

  async function reload(renew: boolean): Promise<void> {
    const { request, signal } = begin();
    loading = true;
    loadingMore = false;
    error = "";
    moreError = "";
    items = [];
    nextOffset = 0;
    exhausted = false;
    try {
      if (renew) {
        asOf = "";
        // A horizon needs the daemon date before its cutoff can be sent.
        if (filters.expiringDays) {
          const seed = await listContracts({ limit: 1 }, { session, signal });
          if (request !== generation) return;
          asOf = seed.as_of;
        }
      }
      if (!(await fetchPages(request, signal))) return;
      if (renew && selectedID) detailRevision += 1;
    } catch (cause) {
      if (request !== generation) return;
      items = [];
      error = failed(cause);
    } finally {
      if (request === generation) loading = false;
    }
  }

  async function loadMore(): Promise<void> {
    const { request, signal } = begin();
    loadingMore = true;
    moreError = "";
    try {
      await fetchPages(request, signal);
    } catch (cause) {
      if (request === generation) moreError = failed(cause);
    } finally {
      if (request === generation) loadingMore = false;
    }
  }

  function setFilters(next: Partial<ContractFilters>): void {
    filters = { ...filters, ...next };
    selectedID = null;
    void reload(false);
  }

  async function back(id: string): Promise<void> {
    selectedID = null;
    await tick();
    document.querySelector<HTMLElement>(`[data-contract-id="${CSS.escape(id)}"]`)?.focus();
  }
</script>

<DrawerShell
  title="Contracts"
  ariaLabel="Contracts"
  closeLabel="Close Contracts"
  onclose={() => {}}
>
  {#snippet actions()}
    <IconButton
      size="sm"
      ariaLabel="Refresh contracts"
      title="Refresh"
      disabled={loading}
      onclick={() => void reload(true)}
    >
      <RefreshCwIcon size="14" aria-hidden="true" />
    </IconButton>
  {/snippet}

  <div class="tool-body contracts-body">
    {#if selectedID}
      {#key `${selectedID}:${detailRevision}`}
        <ContractDetail
          {session}
          contractID={selectedID}
          {asOf}
          {onauthfailure}
          onback={(id) => void back(id)}
        />
      {/key}
    {:else}
      <div class="filters" role="group" aria-label="Contract filters">
        <SelectDropdown
          title="Status"
          disabled={!asOf}
          value={filters.status}
          options={contractStatusOptions}
          onchange={(value) => setFilters({ status: value })}
        />
        <SelectDropdown
          title="Type"
          disabled={!asOf}
          value={filters.type}
          options={typeOptions}
          onchange={(value) => setFilters({ type: value })}
        />
        <SelectDropdown
          title="Expires"
          disabled={!asOf}
          value={String(filters.expiringDays)}
          options={expiryOptions}
          onchange={(value) => setFilters({ expiringDays: Number(value) as ExpiryWindow })}
        />
        {#if filtersSet(filters)}<Button
            size="sm"
            onclick={() => setFilters({ ...noContractFilters })}>Clear filters</Button
          >{/if}
      </div>
      {#if asOf}
        <p class="tool-muted">
          {#if filters.expiringDays}
            Shows agreements expiring from {formatCivilDate(asOf)} through {formatCivilDate(
              addCivilDays(asOf, filters.expiringDays),
            )}{filters.status === "active"
              ? "."
              : ", in any status. Choose Active to see only current agreements."}
          {:else}
            Dates as of {formatCivilDate(asOf)}.
          {/if}
        </p>
      {/if}

      {#if loading}
        <p class="tool-muted" role="status"><Spinner size={14} /> Loading contracts…</p>
      {:else if error}
        <div class="tool-heading">
          <p class="tool-error" role="alert">{error}</p>
          <Button size="sm" onclick={() => void reload(!asOf)}>Try again</Button>
        </div>
      {:else if rows.length === 0 && filtersSet(filters)}
        <div class="tool-heading">
          <p class="tool-muted">No contracts match these filters.</p>
          <Button size="sm" onclick={() => setFilters({ ...noContractFilters })}
            >Clear filters</Button
          >
        </div>
      {:else if rows.length === 0}
        <p class="tool-muted">
          No contracts yet. Register one with <code>docbank contract create "&lt;title&gt;"</code>.
        </p>
      {:else}
        <Table bare unsorted ariaLabel="Contracts" class="contracts">
          <thead>
            <tr>
              <th scope="col">Title</th>
              <th scope="col" class="narrow-hidden">Type</th>
              <th scope="col">Counterparty</th>
              <th scope="col">Status</th>
              <th scope="col" class="narrow-hidden">Starts</th>
              <th scope="col">Expires</th>
            </tr>
          </thead>
          <tbody>
            {#each rows as contract (contract.contract_id)}
              <tr>
                <th scope="row" class="title-cell">
                  <button
                    type="button"
                    class="title-button"
                    data-contract-id={contract.contract_id}
                    aria-label={`Show contract ${contract.title}`}
                    onclick={() => (selectedID = contract.contract_id)}>{contract.title}</button
                  >
                </th>
                <td class="narrow-hidden">{contractTypeLabel(contract.contract_type)}</td>
                <td class="wrap-cell">{counterpartyNames(contract)}</td>
                <td
                  ><Chip size="xs" tone={statusTone(contract.effective_status)}
                    >{contractStatusLabel(contract.effective_status)}</Chip
                  ></td
                >
                <td class="narrow-hidden">{formatCivilDate(contract.starts_on)}</td>
                <td class="date-cell">{formatCivilDate(contract.expires_on)}</td>
              </tr>
            {/each}
          </tbody>
        </Table>
      {/if}
      {#if !loading && !error && !exhausted}
        <div class="tool-heading">
          <Button size="sm" disabled={loadingMore} onclick={() => void loadMore()}
            >{loadingMore ? "Loading…" : "Load more"}</Button
          >
          {#if moreError}<p class="tool-error" role="alert">{moreError}</p>{/if}
        </div>
      {/if}
    {/if}
  </div>
</DrawerShell>

<style>
  /* The page's single column never grows past the viewport to fit the
     table's content, so the filters wrap and the table cells wrap. */
  .contracts-body {
    grid-template-columns: minmax(0, 1fr);
  }
  .filters {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-4);
  }
  :global(.contracts) {
    width: 100%;
  }
  :global(.contracts) td,
  :global(.contracts) th {
    white-space: nowrap;
  }
  :global(.contracts) .title-cell,
  :global(.contracts) .wrap-cell {
    white-space: normal;
    font-weight: normal;
  }
  .title-button {
    padding: 0;
    border: 0;
    background: transparent;
    color: var(--accent-blue);
    font: inherit;
    text-align: left;
    cursor: pointer;
  }
  .title-button:hover {
    text-decoration: underline;
  }
  .title-button:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: 2px;
  }
  @media (max-width: 760px) {
    .narrow-hidden {
      display: none;
    }
    /* Four columns must fit a phone: narrower cell spacing than the kit
       table's 10px, and the expiry date may break after its comma. */
    :global(.contracts) thead th,
    :global(.contracts) tbody th,
    :global(.contracts) tbody td {
      padding-inline: var(--space-2);
    }
    :global(.contracts) tbody .date-cell {
      white-space: normal;
    }
  }
</style>
