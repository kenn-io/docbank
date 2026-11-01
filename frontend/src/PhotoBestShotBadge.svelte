<script lang="ts">
  import type { PhotoBestShotProposal } from "./generated/docbank.js";
  let { proposal, compact = false }: { proposal?: PhotoBestShotProposal; compact?: boolean } =
    $props();
</script>

{#if proposal}<span
    class="proposal"
    class:compact
    class:keep={proposal.flag === "pick"}
    title={`${proposal.rationale} · ${Math.round(proposal.confidence * 100)}% confidence`}
    aria-label={proposal.flag === "pick" ? "Suggested keep" : "Suggested cut"}
    >{proposal.flag === "pick" ? "KEEP" : "CUT"}</span
  >{/if}

<style>
  .proposal {
    display: inline-block;
    background: var(--bg-surface);
    border: 1px solid var(--accent-red);
    color: var(--accent-red);
    border-radius: var(--radius-sm);
    padding: 2px 6px;
    font-size: var(--font-size-xs);
    white-space: nowrap;
  }
  .proposal.keep {
    color: var(--accent-green);
    border-color: var(--accent-green);
  }
  .proposal.compact {
    padding: 1px 4px;
  }
</style>
