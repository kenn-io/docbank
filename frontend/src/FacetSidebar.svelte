<script lang="ts">
  import { canonicalQuery, parseQuery, type Query, type QueryFilters } from "./query.js";
  import type { SnapshotPage } from "./snapshots.js";

  type Facet = SnapshotPage["facets"][number];
  type FacetValue = Facet["values"][number];

  interface Props {
    facets: SnapshotPage["facets"];
    query: Query;
    disabled?: boolean;
    onchange: (query: Query) => void;
  }

  let { facets, query, disabled = false, onchange }: Props = $props();

  const names: Record<Facet["dimension"], string> = {
    collections: "Collections", tags: "Tags", media_family: "Media family", extension: "Extension",
    modified: "Modified", size: "Size", text_coverage: "Text coverage", duplicates: "Duplicates",
  };
  const arrayFields = {
    collections: "collection_ids", tags: "tag_ids", media_family: "media_families",
    extension: "extensions", text_coverage: "text_coverage",
  } as const;
  const sizeRanges: Record<string, { min?: number; max?: number; label: string }> = {
    lt_1_mib: { max: (1 << 20) - 1, label: "Less than 1 MiB" },
    "1_mib_to_10_mib": { min: 1 << 20, max: (10 << 20) - 1, label: "1 MiB to 10 MiB" },
    "10_mib_to_100_mib": { min: 10 << 20, max: (100 << 20) - 1, label: "10 MiB to 100 MiB" },
    "100_mib_to_1_gib": { min: 100 << 20, max: (1 << 30) - 1, label: "100 MiB to 1 GiB" },
    gte_1_gib: { min: 1 << 30, label: "1 GiB or more" },
  };

  function changed(filters: QueryFilters): Query {
    return parseQuery(canonicalQuery({ ...query, filters }));
  }

  function toggleArray(dimension: keyof typeof arrayFields, key: string): void {
    const field = arrayFields[dimension];
    const current = (query.filters[field] as string[] | undefined) ?? [];
    const values = current.includes(key) ? current.filter((value) => value !== key) : [...current, key];
    const filters = { ...query.filters, [field]: values.length ? values : undefined };
    if (dimension === "tags" && values.length) filters.no_tags = undefined;
    onchange(changed(filters));
  }

  function monthRange(key: string): { after: string; before: string } | undefined {
    const match = /^(\d{4})-(0[1-9]|1[0-2])$/.exec(key);
    if (!match) return undefined;
    const year = Number(match[1]);
    const month = Number(match[2]) - 1;
    const after = new Date(Date.UTC(year, month, 1)).toISOString();
    const before = new Date(Date.UTC(year, month + 1, 1)).toISOString();
    return { after, before };
  }

  function selected(dimension: Facet["dimension"], value: FacetValue): boolean {
    if (dimension === "modified") {
      const range = monthRange(value.key);
      return Boolean(range && query.filters.modified_after === range.after && query.filters.modified_before === range.before);
    }
    if (dimension === "size") {
      const range = sizeRanges[value.key];
      return Boolean(range && query.filters.size_min === range.min && query.filters.size_max === range.max);
    }
    if (dimension === "duplicates") return value.key === "duplicate" && query.filters.has_duplicates === true;
    return value.selected;
  }

  function label(dimension: Facet["dimension"], value: FacetValue): string {
    if (dimension === "modified") {
      const range = monthRange(value.key);
      if (range) return new Intl.DateTimeFormat("en-US", { timeZone: "UTC", month: "long", year: "numeric" }).format(new Date(range.after));
    }
    if (dimension === "size" && sizeRanges[value.key]) return sizeRanges[value.key].label;
    return value.label.replaceAll("_", " ");
  }

  function apply(dimension: Facet["dimension"], value: FacetValue): void {
    if (dimension in arrayFields) {
      toggleArray(dimension as keyof typeof arrayFields, value.key);
      return;
    }
    if (dimension === "modified") {
      const range = monthRange(value.key);
      if (!range) return;
      const active = selected(dimension, value);
      onchange(changed({ ...query.filters, modified_after: active ? undefined : range.after, modified_before: active ? undefined : range.before }));
      return;
    }
    if (dimension === "size") {
      const range = sizeRanges[value.key];
      if (!range) return;
      const active = selected(dimension, value);
      onchange(changed({ ...query.filters, size_min: active ? undefined : range.min, size_max: active ? undefined : range.max }));
      return;
    }
    if (dimension === "duplicates" && value.key === "duplicate") {
      onchange(changed({ ...query.filters, has_duplicates: query.filters.has_duplicates ? undefined : true }));
    }
  }

  function actionable(dimension: Facet["dimension"], value: FacetValue): boolean {
    return dimension in arrayFields || (dimension === "modified" && monthRange(value.key) !== undefined) ||
      (dimension === "size" && sizeRanges[value.key] !== undefined) ||
      (dimension === "duplicates" && value.key === "duplicate");
  }

  function toggleMissingTags(): void {
    const enable = !query.filters.no_tags;
    onchange(changed({ ...query.filters, no_tags: enable || undefined, tag_ids: enable ? undefined : query.filters.tag_ids }));
  }
</script>

<aside class="facets" aria-label="Snapshot facets">
  <h2>Refine snapshot</h2>
  {#each facets as facet (facet.dimension)}
    <section role="group" aria-label={`${names[facet.dimension]} facet`}>
      <h3>{names[facet.dimension]}</h3>
      {#if !facet.available}
        <p class="unavailable">Unavailable · {facet.reason?.replaceAll("_", " ")}</p>
      {:else}
        <div class="values">
          {#each facet.values as value (value.key)}
            {@const valueLabel = label(facet.dimension, value)}
            {#if actionable(facet.dimension, value)}
              <button type="button" class:selected={selected(facet.dimension, value)}
                aria-pressed={selected(facet.dimension, value)} disabled={disabled}
                aria-label={`${valueLabel}, ${value.count} documents${selected(facet.dimension, value) ? ", selected" : ""}`}
                onclick={() => apply(facet.dimension, value)}>
                <span>{valueLabel}</span><strong>{value.count}</strong>
              </button>
            {:else}
              <span class="informational">{valueLabel} · {value.count} · informational</span>
            {/if}
          {/each}
        </div>
        <div class="summary">
          {#if facet.dimension === "tags"}
            <button type="button" class:selected={query.filters.no_tags === true} aria-pressed={query.filters.no_tags === true}
              disabled={disabled} onclick={toggleMissingTags}>Missing {facet.missing}</button>
          {:else}<span>Missing {facet.missing}</span>{/if}
          <span>Other {facet.other}</span>
        </div>
      {/if}
    </section>
  {/each}
</aside>

<style>
  .facets { display: grid; align-content: start; gap: var(--space-4); min-width: 0; padding: var(--space-4); border-right: 1px solid var(--border-default); background: var(--bg-inset); }
  h2, h3, p { margin: 0; }
  h2 { font-size: var(--font-size-md); }
  h3 { margin-bottom: var(--space-2); color: var(--text-muted); font-size: var(--font-size-xs); letter-spacing: .04em; text-transform: uppercase; }
  .values { display: grid; gap: var(--space-1); }
  button { display: flex; align-items: center; justify-content: space-between; gap: var(--space-2); width: 100%; min-height: 30px; padding: var(--space-1) var(--space-2); border: 1px solid var(--border-default); border-radius: var(--radius-md); background: var(--bg-surface); color: var(--text-secondary); text-align: left; cursor: pointer; }
  button:hover:not(:disabled), button.selected { border-color: var(--accent-blue); color: var(--text-primary); }
  button.selected { background: color-mix(in srgb, var(--accent-blue) 10%, var(--bg-surface)); }
  button:focus-visible { outline: 2px solid var(--accent-blue); outline-offset: 1px; }
  button:disabled { cursor: not-allowed; opacity: var(--opacity-disabled); }
  strong { font-variant-numeric: tabular-nums; }
  .summary { display: flex; flex-wrap: wrap; gap: var(--space-2); margin-top: var(--space-2); color: var(--text-muted); font-size: var(--font-size-xs); }
  .summary button { width: auto; min-height: 24px; font-size: inherit; }
  .informational, .unavailable { color: var(--text-muted); font-size: var(--font-size-xs); overflow-wrap: anywhere; }
  @media (max-width: 900px) { .facets { grid-template-columns: repeat(2, minmax(0, 1fr)); border-right: 0; border-bottom: 1px solid var(--border-default); } .facets > h2 { grid-column: 1 / -1; } }
  @media (max-width: 640px) { .facets { grid-template-columns: minmax(0, 1fr); } }
</style>
