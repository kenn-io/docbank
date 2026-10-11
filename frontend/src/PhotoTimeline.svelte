<script lang="ts">
  import { tick } from "svelte";
  import { Button, Spinner } from "@kenn-io/kit-ui";
  import { monthLabel } from "./photoGrid.js";
  import { timelineYears, type CaptureDayFacet } from "./photoTimeline.js";

  let { facet, loading, error, selected, onselect, onretry }: { facet?: CaptureDayFacet; loading: boolean; error: string; selected?: string; onselect: (date: string) => void; onretry: () => void } = $props();
  let monthStrip = $state<HTMLElement>();
  const years = $derived(facet?.available ? timelineYears(facet) : []);
  const months = $derived(years.flatMap(year => year.months));
  const focused = $derived(months.find(month => selected?.length === 4 ? month.key.startsWith(selected) : month.key === selected?.slice(0, 7)) ?? months[0]);
  const peak = $derived(Math.max(1, ...years.map(year => year.count)));
  $effect(() => {
    focused?.key; selected;
    void tick().then(() => {
      monthStrip?.querySelector<HTMLElement>(".timeline-active")?.scrollIntoView?.({ block: "nearest", inline: "nearest" });
    });
  });
</script>

<section class="photo-timeline" aria-label="Photo timeline">
  {#if loading}
    <div role="status"><Spinner size={14} /> Loading timeline...</div>
  {:else if error}
    <div role="alert">{error} <Button size="sm" onclick={onretry}>Retry timeline</Button></div>
  {:else if facet && !facet.available}
    <div role="status">{facet.reason === "snapshot_busy" || facet.reason === "snapshot_capacity" ? "Timeline is busy. Retry in a moment." : facet.reason === "time_budget_exceeded" ? "Timeline took too long. Try again." : facet.reason === "member_budget_exceeded" ? "Too many photos for timeline counts." : facet.reason === "snapshot_too_large" ? "Timeline exceeds its limits." : "Timeline counts are unavailable."} <Button size="sm" onclick={onretry}>Retry timeline</Button></div>
  {:else if facet?.available}
    <div class="timeline-summary">{facet.total?.toLocaleString()} {facet.total === 1 ? "photo" : "photos"} in scope · {facet.missing?.toLocaleString()} undated</div>
    {#if years.length}
      <nav class="year-ribbon" aria-label="Timeline years">
        {#each years as year (year.key)}
          <button type="button" class="photo-toggle kit-button kit-control-states kit-button--sm" aria-pressed={Boolean(selected?.startsWith(year.key))} onclick={() => onselect(year.key)}>
            <span class="year-count">{year.key}<span>{year.count.toLocaleString()}</span><span class="year-density" style:width={`${Math.max(5, year.count / peak * 100)}%`}></span></span>
          </button>
        {/each}
      </nav>
      <nav class="month-sections scrubber" aria-label="Timeline month scrubber" bind:this={monthStrip}>
        {#each years as year (year.key)}
          {#if focused?.key.startsWith(year.key)}
            {#each year.months as month (month.key)}<button type="button" class="photo-toggle kit-button kit-control-states kit-button--sm" class:timeline-active={focused?.key === month.key} aria-pressed={Boolean(selected?.startsWith(month.key))} onclick={() => onselect(month.key)}>{monthLabel(month.key)} · {month.count.toLocaleString()}</button>{/each}
          {/if}
        {/each}
      </nav>
      {#if focused}
        <div class="day-rows" aria-label={monthLabel(focused.key)}>
          {#each focused.days as day (day.key)}
            <button type="button" class="photo-toggle kit-button kit-control-states kit-button--sm" aria-pressed={selected === day.key} onclick={() => onselect(day.key)}>{day.key} · {day.count.toLocaleString()} {day.count === 1 ? "photo" : "photos"}</button>
          {/each}
        </div>
      {/if}
    {:else if facet.total === 0}
      <p>No photos in this scope.</p>
    {:else}
      <p>These photos have no recorded capture dates.</p>
    {/if}
  {/if}
</section>

<style>
  .photo-timeline { flex-shrink: 0; max-height: 42vh; overflow: auto; padding: var(--space-3) var(--space-5); border-bottom: 1px solid var(--border-default); color: var(--text-primary); background: var(--bg-inset); font-size: var(--font-size-sm); }
  .timeline-summary { color: var(--text-muted); margin-bottom: var(--space-2); }
  .year-ribbon, .day-rows { display: flex; flex-wrap: wrap; gap: var(--space-2); align-items: center; }
  .scrubber { display: flex; gap: var(--space-2); overflow-x: auto; padding: var(--space-1) 0; }
  .scrubber :global(button) { flex-shrink: 0; }
  .year-ribbon { margin-bottom: var(--space-3); }
  .year-count { display: flex; flex-direction: column; min-width: 58px; align-items: start; gap: var(--space-1); }
  .year-count > span:first-child { font-size: var(--font-size-xs); }
  .year-density { height: 3px; background: currentColor; border-radius: 2px; }
  .month-sections { margin: var(--space-2) 0; }
  .day-rows { border-top: 1px solid var(--border-default); padding-top: var(--space-2); }
</style>
