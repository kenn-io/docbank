<script lang="ts">
  import InspectorDetails, { type InspectorFact } from "./InspectorDetails.svelte";

  // Identifiers and hashes: copyable when needed, out of the way otherwise.
  let {
    facts,
    summary = "Details",
    open = false,
  }: { facts: InspectorFact[]; summary?: string; open?: boolean } = $props();
</script>

<details class="technical" aria-label={summary} {open}>
  <summary>{summary}</summary>
  <InspectorDetails {facts} label={summary === "Details" ? "Technical details" : summary} />
</details>

<style>
  summary {
    width: fit-content;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    cursor: pointer;
  }
  details[open] > summary {
    margin-bottom: var(--space-4);
  }
</style>
