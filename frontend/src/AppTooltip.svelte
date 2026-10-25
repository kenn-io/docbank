<script module lang="ts">
  // One hover delay for every tooltip, so a pointer crossing the page doesn't flash bubbles.
  export const TOOLTIP_DELAY_MS = 700;
</script>

<script lang="ts">
  import type { Snippet } from "svelte";
  import { Tooltip } from "@kenn-io/kit-ui";

  // A control with nothing to say keeps its wrapper, so gaining or losing a
  // reason never remounts the control; app.css hides the empty bubble.
  let {
    text = "",
    side = "bottom",
    align = "center",
    focusable = false,
    children,
  }: {
    text?: string;
    side?: "bottom" | "right";
    align?: "start" | "center" | "end";
    focusable?: boolean;
    children: Snippet;
  } = $props();
</script>

<Tooltip {text} {side} {align} {focusable} openDelayMs={TOOLTIP_DELAY_MS}
  >{@render children()}</Tooltip
>
