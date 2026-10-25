<script lang="ts">
  import type { Snippet } from "svelte";
  import { Tooltip } from "@kenn-io/kit-ui";
  import { TOOLTIP_DELAY_MS } from "./tooltip.js";

  // The app's only tooltip. A control with nothing to say keeps its wrapper, so
  // gaining or losing a reason never remounts the control; app.css hides the
  // empty bubble.
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
