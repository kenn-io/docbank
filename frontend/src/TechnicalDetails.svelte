<script lang="ts">
  import { Disclosure } from "@kenn-io/kit-ui";
  import InspectorDetails, { type InspectorFact } from "./InspectorDetails.svelte";

  // Identifiers and hashes: copyable when needed, out of the way otherwise.
  let {
    facts,
    summary = "Details",
    open = false,
  }: { facts: InspectorFact[]; summary?: string; open?: boolean } = $props();
</script>

<div role="group" aria-label={summary}>
  <Disclosure variant="plain" {open} title={summary}>
    <InspectorDetails {facts} label={summary === "Details" ? "Technical details" : summary} />
  </Disclosure>
</div>
