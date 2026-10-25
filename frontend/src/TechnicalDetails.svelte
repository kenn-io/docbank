<script lang="ts">
  import InspectorDetails, { type InspectorFact } from "./InspectorDetails.svelte";

  // Identifiers and hashes: copyable when needed, out of the way otherwise.
  let { facts }: { facts: InspectorFact[] } = $props();
</script>

<details class="technical">
  <summary>Details</summary>
  <InspectorDetails {facts} label="Technical details" />
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
