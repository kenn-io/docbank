<script lang="ts">
  import { Button, SelectDropdown, Spinner } from "@kenn-io/kit-ui";
  import { timelineYears, type CaptureDayFacet } from "./photoTimeline.js";

  let { facet, loading, error, selected, onselect, onretry }: { facet?: CaptureDayFacet; loading: boolean; error: string; selected?: string; onselect: (day: string) => void; onretry: () => void } = $props();
  let focus = $state("");
  const years = $derived(facet?.available ? timelineYears(facet) : []);
  const months = $derived(years.flatMap(year => year.months));
  const focused = $derived(months.find(month => month.key === focus) ?? months.find(month => month.key === selected?.slice(0, 7)) ?? months[0]);
  const peak = $derived(Math.max(1, ...years.map(year => year.count)));
  const monthName = (key: string) => new Intl.DateTimeFormat(undefined, { month: "long", year: "numeric", timeZone: "UTC" }).format(new Date(`${key}-01T00:00:00Z`));
</script>

<section class="photo-timeline" aria-label="Photo timeline">
  {#if loading}
    <div role="status"><Spinner size={14} /> Loading timeline...</div>
  {:else if error}
    <div role="alert">{error} <Button size="sm" onclick={onretry}>Retry timeline</Button></div>
  {:else if facet && !facet.available}
    <div role="status">Timeline counts are unavailable for this scope. Try a smaller scope. <Button size="sm" onclick={onretry}>Retry timeline</Button></div>
  {:else if facet?.available}
    <div class="timeline-summary">{facet.total?.toLocaleString()} photos in scope · {facet.missing?.toLocaleString()} undated</div>
    {#if years.length}
      <nav class="year-ribbon" aria-label="Timeline years">
        {#each years as year (year.key)}
          <Button size="sm" tone={focused?.key.startsWith(year.key) ? "info" : "neutral"} onclick={() => focus = year.months[0].key}>
            <span class="year-count">{year.key}<span>{year.count.toLocaleString()}</span><span class="year-density" style:width={`${Math.max(5, year.count / peak * 100)}%`}></span></span>
          </Button>
        {/each}
      </nav>
      <div class="timeline-controls">
        <SelectDropdown title="Timeline month" value={focused?.key ?? ""} options={months.map(month => ({ value: month.key, label: `${monthName(month.key)} · ${month.count.toLocaleString()}` }))} onchange={value => focus = value} />
        <SelectDropdown title="Timeline day" value={selected ?? ""} options={focused?.days.map(day => ({ value: day.key, label: `${day.key} · ${day.count.toLocaleString()} photos` })) ?? []} onchange={onselect} />
      </div>
      <div class="month-sections" aria-label="Timeline months">
        {#each years as year (year.key)}
          {#if focused?.key.startsWith(year.key)}
            <div class="year-months"><span>{year.key}</span>{#each year.months as month (month.key)}<Button size="sm" tone={focused?.key === month.key ? "info" : "neutral"} onclick={() => focus = month.key}>{monthName(month.key)} · {month.count.toLocaleString()}</Button>{/each}</div>
          {/if}
        {/each}
      </div>
      {#if focused}
        <div class="day-rows" aria-label={monthName(focused.key)}>
          {#each focused.days as day (day.key)}
            <Button size="sm" tone={selected === day.key ? "info" : "neutral"} onclick={() => onselect(day.key)}>{day.key} · {day.count.toLocaleString()} photos</Button>
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
  .year-ribbon, .timeline-controls, .day-rows, .year-months { display: flex; flex-wrap: wrap; gap: var(--space-2); align-items: center; }
  .year-ribbon { margin-bottom: var(--space-3); }
  .year-count { display: flex; flex-direction: column; min-width: 58px; align-items: start; gap: var(--space-1); }
  .year-count > span:first-child { font-size: var(--font-size-xs); }
  .year-density { height: 3px; background: currentColor; border-radius: 2px; }
  .month-sections { margin: var(--space-2) 0; }
  .year-months > span { color: var(--text-muted); min-width: 40px; }
  .day-rows { border-top: 1px solid var(--border-default); padding-top: var(--space-2); }
</style>
