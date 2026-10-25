<script module lang="ts">
  export type InspectorFact = {
    label: string;
    value: string | number | undefined | null;
    copy?: boolean;
    mono?: boolean;
  };
</script>

<script lang="ts">
  import CopyValue from "./CopyValue.svelte";
  let { facts, label = "Details" }: { facts: InspectorFact[]; label?: string } = $props();
</script>

<dl aria-label={label} class="inspector-details">
  {#each facts.filter((fact) => fact.value !== undefined && fact.value !== null && fact.value !== "") as fact (fact.label)}
    <div>
      <dt>{fact.label}</dt>
      <dd>
        {#if fact.copy}<CopyValue
            label={fact.label}
            value={String(fact.value)}
            mono={fact.mono ?? true}
          />{:else}{fact.value}{/if}
      </dd>
    </div>
  {/each}
</dl>

<style>
  /* One label column for every row, as wide as its longest label. */
  .inspector-details {
    display: grid;
    grid-template-columns: minmax(76px, max-content) minmax(0, 1fr);
    gap: var(--space-4) var(--space-3);
    align-items: baseline;
    margin: 0;
    min-width: 0;
  }
  .inspector-details > div {
    display: contents;
  }
  dt {
    white-space: nowrap;
  }
  dt,
  dd {
    margin: 0;
    font-size: var(--font-size-sm);
    line-height: 1.5;
  }
  dt {
    color: var(--text-muted);
  }
  dd {
    color: var(--text-primary);
    min-width: 0;
    overflow-wrap: anywhere;
  }
</style>
