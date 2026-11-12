<script lang="ts">
  import type { Snippet } from "svelte";
  import { Table } from "@kenn-io/kit-ui";

  let {
    ariaLabel,
    compact = false,
    header: tableHeader,
    children: rows,
  }: {
    ariaLabel: string;
    compact?: boolean;
    header: Snippet;
    children: Snippet;
  } = $props();
</script>

<div class="target-list" class:compact>
  <Table unsorted {ariaLabel} class="target-table" zebra={false} scroll="page">
    {#snippet header()}{@render tableHeader()}{/snippet}
    {#snippet children()}{@render rows()}{/snippet}
  </Table>
</div>

<style>
  .target-list {
    overflow: auto;
    max-height: 240px;
  }
  .target-list :global(.target-table .kit-table) {
    table-layout: fixed;
    font-size: var(--font-size-xs);
  }
  .target-list :global(.target-table th),
  .target-list :global(.target-table td) {
    padding: var(--space-2);
    text-align: left;
    overflow-wrap: anywhere;
    border-bottom: 1px solid var(--border-default);
  }
  .compact :global(.target-table th:first-child),
  .compact :global(.target-table td:first-child) {
    padding-inline-start: 0;
  }
  .compact :global(.target-table .kit-table) {
    margin-block-end: 0;
  }
</style>
