<script lang="ts">
  import CheckIcon from "@lucide/svelte/icons/check";
  import ChevronDownIcon from "@lucide/svelte/icons/chevron-down";
  import {
    autoReposition,
    dismissable,
    floatingPopoverStyle,
  } from "@kenn-io/kit-ui";
  import { tick } from "svelte";
  import type { Tag } from "./api.js";
  import { groupTags } from "./tagPresentation.js";

  interface PickerOption {
    id: string;
    label: string;
    fullName: string;
    count?: number;
    color?: string;
    tag?: Tag;
  }

  interface Props {
    value: string;
    tags: readonly Tag[];
    title: string;
    placeholder: string;
    onchange: (value: string) => void;
    includeAll?: boolean;
    disabled?: boolean;
    tooltip?: string;
    align?: "start" | "end";
    class?: string;
  }

  let {
    value,
    tags,
    title,
    placeholder,
    onchange,
    includeAll = false,
    disabled = false,
    tooltip,
    align = "start",
    class: className = "",
  }: Props = $props();

  let open = $state(false);
  let highlightedIndex = $state(0);
  let containerEl = $state<HTMLDivElement>();
  let buttonEl = $state<HTMLButtonElement>();
  let listEl = $state<HTMLDivElement>();
  let listStyle = $state("");

  const dropdownID = $props.id();
  const listboxID = `${dropdownID}-listbox`;
  const groups = $derived(groupTags(tags));
  const options = $derived.by(() => {
    const items: PickerOption[] = includeAll
      ? [{ id: "", label: placeholder, fullName: placeholder }]
      : [];
    for (const group of groups) {
      for (const item of group.tags) {
        items.push({
          id: item.tag.id,
          label: item.label,
          fullName: item.tag.name,
          count: item.tag.assignment_count,
          color: item.color,
          tag: item.tag,
        });
      }
    }
    return items;
  });
  const optionIndexes = $derived(
    new Map(options.map((option, index) => [option.id, index])),
  );
  const selected = $derived(options.find((option) => option.id === value));
  const triggerText = $derived(selected?.label ?? placeholder);
  const triggerFullName = $derived(selected?.fullName ?? placeholder);
  const trulyDisabled = $derived(disabled || options.length === 0);

  $effect(() => {
    if (!open) return;
    const cleanups = [
      dismissable({
        owners: () => [containerEl],
        dismiss: close,
        escapeFocus: () => buttonEl,
      }),
      autoReposition(() => listEl, positionList),
    ];
    return () => cleanups.forEach((cleanup) => cleanup());
  });

  $effect(() => {
    if (trulyDisabled && open) close();
  });

  function optionID(index: number): string {
    return `${dropdownID}-option-${index}`;
  }

  function positionList(): void {
    if (!buttonEl || !listEl) return;
    const trigger = buttonEl.getBoundingClientRect();
    const width = Math.max(listEl.offsetWidth, trigger.width);
    listStyle = `${floatingPopoverStyle({
      trigger,
      viewportWidth: window.innerWidth,
      viewportHeight: window.innerHeight,
      popoverWidth: width,
      popoverHeight: listEl.offsetHeight,
      align,
      triggerGap: 2,
    })}; min-width: ${Math.round(trigger.width)}px`;
  }

  function close(): void {
    open = false;
  }

  async function show(): Promise<void> {
    if (trulyDisabled) return;
    open = true;
    highlightedIndex = Math.max(0, optionIndexes.get(value) ?? 0);
    await tick();
    positionList();
    document
      .getElementById(optionID(highlightedIndex))
      ?.scrollIntoView({ block: "nearest" });
  }

  function toggle(): void {
    if (open) close();
    else void show();
  }

  function select(option: PickerOption): void {
    if (trulyDisabled) return;
    onchange(option.id);
    close();
    buttonEl?.focus();
  }

  function moveHighlight(delta: number): void {
    if (options.length === 0) return;
    highlightedIndex =
      (highlightedIndex + delta + options.length) % options.length;
    document
      .getElementById(optionID(highlightedIndex))
      ?.scrollIntoView({ block: "nearest" });
  }

  function onFocusout(event: FocusEvent): void {
    const nextTarget = event.relatedTarget as Node | null;
    if (nextTarget && containerEl?.contains(nextTarget)) return;
    close();
  }

  function onButtonKeydown(event: KeyboardEvent): void {
    if (event.key === "Tab") {
      close();
      return;
    }
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      if (!open) void show();
      else moveHighlight(event.key === "ArrowDown" ? 1 : -1);
      return;
    }
    if (open && (event.key === "Home" || event.key === "End")) {
      event.preventDefault();
      highlightedIndex = event.key === "Home" ? 0 : options.length - 1;
      document
        .getElementById(optionID(highlightedIndex))
        ?.scrollIntoView({ block: "nearest" });
      return;
    }
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      if (!open) {
        void show();
        return;
      }
      const option = options[highlightedIndex];
      if (option) select(option);
    }
  }
</script>

{#snippet optionRow(option: PickerOption, index: number)}
  <button
    id={optionID(index)}
    type="button"
    tabindex="-1"
    class="tag-picker__option"
    class:highlighted={index === highlightedIndex}
    class:selected={option.id === value}
    role="option"
    aria-label={option.count === undefined
      ? option.fullName
      : `${option.fullName} (${option.count})`}
    aria-selected={option.id === value}
    title={option.tag ? option.fullName : undefined}
    onclick={() => select(option)}
    onmouseenter={() => (highlightedIndex = index)}
  >
    {#if option.color}
      <span
        class="tag-picker__swatch"
        data-tag-swatch
        style:background-color={option.color}
        aria-hidden="true"
      ></span>
    {:else}
      <span class="tag-picker__swatch-placeholder" aria-hidden="true"></span>
    {/if}
    <span class="tag-picker__option-label">{option.label}</span>
    {#if option.count !== undefined}
      <span class="tag-picker__count">{option.count}</span>
    {/if}
    <span class="tag-picker__check">
      {#if option.id === value}
        <CheckIcon size="12" strokeWidth="2.2" aria-hidden="true" />
      {/if}
    </span>
  </button>
{/snippet}

<div
  class={["tag-picker", className]}
  bind:this={containerEl}
  onfocusout={onFocusout}
>
  <!-- kit-ui-check-ignore: pinned kit dropdowns cannot render each inactive tag's color. -->
  <button role="combobox"
    bind:this={buttonEl}
    class="tag-picker__trigger"
    type="button"
    onclick={toggle}
    onkeydown={onButtonKeydown}
    aria-haspopup="listbox"
    aria-expanded={open}
    aria-controls={listboxID}
    aria-activedescendant={open ? optionID(highlightedIndex) : undefined}
    aria-label={`${title}: ${triggerFullName}`}
    title={tooltip ?? `${title}: ${triggerFullName}`}
    disabled={trulyDisabled}
  >
    {#if selected?.color}
      <span
        class="tag-picker__swatch"
        data-tag-swatch
        style:background-color={selected.color}
        aria-hidden="true"
      ></span>
    {/if}
    <span class="tag-picker__value">{triggerText}</span>
    <ChevronDownIcon
      class="tag-picker__chevron"
      size="12"
      strokeWidth="2"
      aria-hidden="true"
    />
  </button>

  {#if open}
    <!-- kit-ui-check-ignore: paired with the scoped color-aware combobox above. -->
    <div role="listbox"
      id={listboxID}
      class="tag-picker__list kit-popover-card"
      style={listStyle}
      bind:this={listEl}
    >
      {#if includeAll && options[0]}
        {@render optionRow(options[0], 0)}
      {/if}
      {#each groups as group}
        <div
          class="tag-picker__group"
          role="group"
          aria-label={group.name ?? "Ungrouped tags"}
        >
          {#if group.name}
            <div class="tag-picker__group-heading">{group.name}</div>
          {/if}
          {#each group.tags as item (item.tag.id)}
            {@const index = optionIndexes.get(item.tag.id) ?? 0}
            {@render optionRow(options[index]!, index)}
          {/each}
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .tag-picker {
    position: relative;
    min-width: 150px;
  }

  /* kit-ui-check-ignore: the pinned select cannot render arbitrary tag swatches. */
  .tag-picker__trigger {
    box-sizing: border-box;
    display: flex;
    align-items: center;
    gap: 6px;
    width: 100%;
    height: 26px;
    padding: 0 8px;
    border: var(--border-width) solid var(--border-muted);
    border-radius: var(--radius-sm);
    background: var(--bg-inset);
    color: var(--text-secondary);
    cursor: pointer;
    font: inherit;
    font-size: var(--font-size-xs);
    font-weight: var(--font-weight-semibold, 600);
    text-align: left;
  }

  .tag-picker__trigger:hover:not(:disabled),
  .tag-picker__trigger[aria-expanded="true"] {
    border-color: var(--border-default);
    color: var(--text-primary);
  }

  .tag-picker__trigger:disabled {
    cursor: default;
    opacity: var(--opacity-disabled);
  }

  .tag-picker__value,
  .tag-picker__option-label {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  :global(.tag-picker__chevron) {
    flex-shrink: 0;
    opacity: 0.55;
  }

  .tag-picker__list {
    position: fixed;
    z-index: var(--z-popover);
    width: max-content;
    max-width: min(340px, calc(100vw - 16px));
    max-height: min(360px, calc(100vh - 16px));
    overflow-y: auto;
    padding: 2px;
  }

  .tag-picker__group + .tag-picker__group {
    border-top: 1px solid var(--border-muted);
  }

  .tag-picker__group-heading {
    padding: 6px 8px 3px;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-weight: var(--font-weight-bold);
    letter-spacing: 0.05em;
    overflow-wrap: anywhere;
    text-transform: uppercase;
  }

  .tag-picker__option {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    width: 100%;
    padding: 5px 8px;
    border: 0;
    border-radius: var(--radius-sm);
    background: transparent;
    color: var(--text-secondary);
    cursor: pointer;
    font: inherit;
    font-size: var(--font-size-xs);
    text-align: left;
  }

  .tag-picker__option.highlighted,
  .tag-picker__option:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .tag-picker__option.selected {
    color: var(--accent-blue);
    font-weight: var(--font-weight-semibold, 600);
  }

  .tag-picker__swatch,
  .tag-picker__swatch-placeholder {
    width: 8px;
    height: 8px;
    flex: 0 0 8px;
    border-radius: var(--radius-dot, 50%);
  }

  .tag-picker__swatch {
    box-shadow: 0 0 0 1px color-mix(in srgb, currentColor 18%, transparent);
  }

  .tag-picker__count {
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-variant-numeric: tabular-nums;
  }

  .tag-picker__check {
    display: inline-flex;
    width: 12px;
    flex: 0 0 12px;
    color: currentColor;
  }
</style>
