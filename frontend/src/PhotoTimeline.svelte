<script lang="ts">
  import { Button, Card, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import { monthLabel } from "./photoGrid.js";
  import { timelineYears, type CaptureDayFacet } from "./photoTimeline.js";

  let {
    facet,
    loading,
    error,
    chosen = "",
    current = "",
    compact = false,
    disabled = false,
    hint = "",
    onselect,
    onretry,
  }: {
    facet?: CaptureDayFacet;
    loading: boolean;
    error: string;
    // The year the user chose, which stays open while the grid scrolls to it.
    chosen?: string;
    // The month of the photo at the top of the grid.
    current?: string;
    compact?: boolean;
    disabled?: boolean;
    hint?: string;
    onselect: (month: string) => void;
    onretry: () => void;
  } = $props();
  const years = $derived(facet?.available ? timelineYears(facet) : []);
  const missing = $derived(facet?.available ? (facet.missing ?? 0) : 0);
  const countLabel = (count: number) => `${count.toLocaleString()} photo${count === 1 ? "" : "s"}`;
  const shortMonth = (key: string) => `${monthLabel(key, { month: "short" })} ${key.slice(0, 4)}`;
  // A year lands on its newest counted month.
  const newest = (key: string) => years.find((year) => year.key === key)?.months[0]?.key ?? key;
  const options = $derived([
    { value: "", label: "Jump to date", disabled: true },
    ...years.flatMap((year) => [
      { value: year.key, label: `${year.key} · ${countLabel(year.count)}` },
      ...year.months.map((month) => ({
        value: month.key,
        label: `${shortMonth(month.key)} · ${countLabel(month.count)}`,
        triggerLabel: shortMonth(month.key),
      })),
    ]),
    ...(missing
      ? [{ value: "undated", label: `${missing.toLocaleString()} undated`, disabled: true }]
      : []),
  ]);
  const selected = $derived(
    years.some((year) => year.months.some((month) => month.key === current)) ? current : "",
  );
</script>

{#snippet problem()}
  <div class="timeline-status" role="alert">
    <span>{error}</span>
    <Button size="sm" ariaLabel="Retry timeline" disabled={loading} onclick={onretry}>Retry</Button>
  </div>
{/snippet}

{#if compact}
  <div class="compact-dates">
    {#if error}{@render problem()}{/if}
    {#if years.length || missing}
      <SelectDropdown
        title="Jump to date"
        value={selected}
        {options}
        {disabled}
        onchange={(value) => onselect(newest(value))}
      />
      {#if hint}<p class="timeline-hint">{hint}</p>{/if}
    {:else if loading}
      <div class="timeline-status" role="status"><Spinner size={14} />Loading dates…</div>
    {/if}
  </div>
{:else}
  <nav aria-label="Timeline">
    <Card level="default" padding="none">
      <div class="rail">
        {#if hint}<p class="timeline-hint">{hint}</p>{/if}
        {#if error}{@render problem()}{/if}
        {#if loading && !years.length && !missing}
          <div class="timeline-status" role="status"><Spinner size={14} />Loading…</div>
        {/if}
        {#each years as year (year.key)}
          {@const open = year.key === chosen || year.key === current.slice(0, 4)}
          <section class="rail-group" class:open>
            <button
              type="button"
              class="rail-row rail-year"
              aria-label={`${year.key}, ${countLabel(year.count)}`}
              {disabled}
              onclick={() => onselect(newest(year.key))}
              ><span>{year.key}</span><small>{year.count.toLocaleString()}</small></button
            >
            {#if open}
              {#each year.months as month (month.key)}
                <button
                  type="button"
                  class="rail-row rail-month"
                  class:active={current === month.key}
                  aria-label={`${shortMonth(month.key)}, ${countLabel(month.count)}`}
                  aria-current={current === month.key ? "true" : undefined}
                  {disabled}
                  onclick={() => onselect(month.key)}
                  ><span>{monthLabel(month.key, { month: "short" })}</span><small
                    >{month.count.toLocaleString()}</small
                  ></button
                >
              {/each}
            {/if}
          </section>
        {/each}
        {#if missing}<p class="rail-foot">{missing.toLocaleString()} undated</p>{/if}
      </div>
    </Card>
  </nav>
{/if}

<style>
  .rail {
    width: 104px;
    max-height: var(--timeline-height, none);
    overflow-y: auto;
  }
  .rail .timeline-status {
    flex-wrap: wrap;
    padding: var(--space-3);
    font-size: var(--font-size-xs);
  }
  .timeline-hint {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
  .rail .timeline-hint {
    padding: var(--space-3);
  }
  .rail-group + .rail-group {
    border-top: 1px solid var(--border-default);
  }
  .rail-group.open {
    background: var(--bg-inset);
    padding-bottom: 4px;
  }
  .rail-foot {
    margin: 0;
    padding: 7px 10px;
    border-top: 1px solid var(--border-default);
    font-size: var(--font-size-xs);
    color: var(--text-muted);
  }
  .rail-row {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 8px;
    width: 100%;
    border: 0;
    border-left: 2px solid transparent;
    background: transparent;
    color: var(--text-secondary);
    text-align: left;
    cursor: pointer;
    font-variant-numeric: tabular-nums;
  }
  .rail-row:disabled {
    cursor: default;
    opacity: var(--opacity-disabled);
  }
  .rail-row small {
    color: var(--text-muted);
    font-size: 11px;
  }
  .rail-row:not(:disabled):hover {
    color: var(--text-primary);
    background: color-mix(in srgb, var(--accent-blue) 6%, transparent);
  }
  .rail-row:focus-visible {
    outline: 2px solid var(--accent-blue);
    outline-offset: -2px;
  }
  .rail-year {
    padding: 7px 10px;
    font-size: var(--font-size-sm);
    font-weight: 600;
  }
  .rail-group.open .rail-year {
    color: var(--text-primary);
  }
  .rail-month {
    padding: 3px 10px 3px 18px;
    font-size: var(--font-size-xs);
  }
  .rail-month.active {
    border-left-color: var(--accent-blue);
    color: var(--accent-blue);
    font-weight: 600;
  }
  .compact-dates {
    display: grid;
    gap: var(--space-2);
    flex-shrink: 0;
    padding: var(--space-3) var(--space-5);
  }
  .compact-dates :global(.kit-select-dropdown) {
    width: 100%;
  }
  .timeline-status {
    display: flex;
    gap: var(--space-2);
    align-items: center;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }
</style>
