<script lang="ts">
  import CheckIcon from "@lucide/svelte/icons/check";
  import ChevronDownIcon from "@lucide/svelte/icons/chevron-down";
  import { TextInput, autoReposition, dismissable, floatingPopoverStyle } from "@kenn-io/kit-ui";
  import { onDestroy, tick } from "svelte";
  import type { Tag } from "./generated/docbank.js";
  import { APIError } from "./api-transport.js";
  import { createTag } from "./receipts.js";
  import { groupTags } from "./tagPresentation.js";
  import AppTooltip from "./AppTooltip.svelte";

  interface PickerOption {
    id: string;
    label: string;
    fullName: string;
    count?: number;
    color?: string;
    tag?: Tag;
    create?: boolean;
  }

  interface Props {
    session?: string;
    contextKey?: string;
    excludedTagIDs?: readonly string[];
    onpick?: (tag: Tag, created: boolean) => void | Promise<void>;
    onauthfailure?: (cause: unknown) => void;
    value: string;
    tags: readonly Tag[];
    title: string;
    placeholder: string;
    onchange: (value: string) => void;
    onmanage?: () => void;
    manageDisabled?: boolean;
    includeAll?: boolean;
    disabled?: boolean;
    tooltip?: string;
    /** Reopen the list after a pick so several tags can be chosen in a row. */
    reopenAfterPick?: boolean;
    align?: "start" | "end";
    class?: string;
  }

  let {
    session,
    contextKey = "",
    excludedTagIDs = [],
    onpick,
    onauthfailure,
    value,
    tags,
    title,
    placeholder,
    onchange,
    onmanage,
    manageDisabled = false,
    includeAll = false,
    disabled = false,
    tooltip,
    reopenAfterPick = true,
    align = "start",
    class: className = "",
  }: Props = $props();

  let query = $state("");
  let createdTags = $state<Tag[]>([]);
  let pending = $state(false);
  let failure = $state("");
  let inputEl = $state<HTMLInputElement>();
  let quietFocus = false;
  let destroyed = false;
  onDestroy(() => {
    destroyed = true;
  });
  const searchable = $derived(session !== undefined);
  const catalog = $derived([
    ...tags,
    ...createdTags.filter((tag) => !tags.some((item) => item.id === tag.id)),
  ]);
  const typedName = $derived(query.trim().normalize("NFC"));
  const filteredTags = $derived(
    catalog.filter(
      (tag) =>
        !excludedTagIDs.includes(tag.id) &&
        (!searchable || tag.name.toLocaleLowerCase().includes(typedName.toLocaleLowerCase())),
    ),
  );
  const canCreate = $derived(
    searchable &&
      typedName !== "" &&
      !catalog.some((tag) => tag.name.normalize("NFC") === typedName),
  );
  let open = $state(false);
  let highlightedIndex = $state(0);
  let containerEl = $state<HTMLDivElement>();
  let buttonEl = $state<HTMLButtonElement>();
  let listEl = $state<HTMLDivElement>();
  let listStyle = $state("");

  const dropdownID = $props.id();
  const listboxID = `${dropdownID}-listbox`;
  const groups = $derived(groupTags(filteredTags));
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
    if (canCreate)
      items.push({
        id: "__create",
        label: `Create “${typedName}”`,
        fullName: `Create “${typedName}”`,
        create: true,
      });
    return items;
  });
  const optionIndexes = $derived(new Map(options.map((option, index) => [option.id, index])));
  const selectedTag = $derived(catalog.find((tag) => tag.id === value));
  const selected = $derived(
    options.find((option) => option.id === value) ??
      (selectedTag
        ? { label: selectedTag.name, fullName: selectedTag.name, color: undefined }
        : undefined),
  );
  const triggerText = $derived(selected?.label ?? placeholder);
  const triggerFullName = $derived(selected?.fullName ?? placeholder);
  const trulyDisabled = $derived(disabled || pending || (!searchable && options.length === 0));

  $effect(() => {
    if (!open) return;
    const cleanups = [
      dismissable({
        owners: () => [containerEl],
        dismiss: close,
        escapeFocus: () => inputEl ?? buttonEl,
      }),
      autoReposition(() => [listEl], positionList),
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
    const anchor = inputEl ?? buttonEl;
    if (!anchor || !listEl) return;
    const trigger = anchor.getBoundingClientRect();
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
    document.getElementById(optionID(highlightedIndex))?.scrollIntoView({ block: "nearest" });
  }

  function toggle(): void {
    if (open) close();
    else void show();
  }

  async function select(option: PickerOption): Promise<void> {
    if (trulyDisabled) return;
    if (!searchable) {
      onchange(option.id);
      close();
      buttonEl?.focus();
      return;
    }
    const requestSession = session!;
    const requestContext = contextKey;
    const name = typedName;
    pending = true;
    failure = "";
    close();
    try {
      const tag = option.create ? await createTag(requestSession, name) : option.tag;
      if (destroyed || session !== requestSession || contextKey !== requestContext || !tag) return;
      if (option.create) createdTags = [...createdTags, tag];
      if (onpick) await onpick(tag, option.create === true);
      else onchange(tag.id);
      if (!destroyed && session === requestSession) query = "";
    } catch (cause) {
      if (destroyed || session !== requestSession || contextKey !== requestContext) return;
      if (cause instanceof APIError && cause.status === 401) onauthfailure?.(cause);
      else failure = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (!destroyed) {
        pending = false;
        await tick();
        quietFocus = !reopenAfterPick;
        inputEl?.focus();
        quietFocus = false;
      }
    }
  }

  function onInput(value: string): void {
    query = value;
    failure = "";
    highlightedIndex = 0;
    open = true;
    void tick().then(positionList);
  }

  function onInputKeydown(event: KeyboardEvent): void {
    if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      if (!open) void show();
      else moveHighlight(event.key === "ArrowDown" ? 1 : -1);
    } else if (event.key === "Enter") {
      event.preventDefault();
      const option = options[highlightedIndex];
      if (open && option) void select(option);
      else void show();
    } else if (event.key === "Escape") {
      if (open) {
        event.stopPropagation();
        close();
      }
    } else if (event.key === "Tab") close();
  }

  function moveHighlight(delta: number): void {
    if (options.length === 0) return;
    highlightedIndex = (highlightedIndex + delta + options.length) % options.length;
    document.getElementById(optionID(highlightedIndex))?.scrollIntoView({ block: "nearest" });
  }

  function onFocusout(event: FocusEvent): void {
    const nextTarget = event.relatedTarget as Node | null;
    if (nextTarget && containerEl?.contains(nextTarget)) return;
    close();
  }

  function onButtonKeydown(event: KeyboardEvent): void {
    if (event.key === "Tab") {
      if (open && onmanage && !manageDisabled && !event.shiftKey) {
        event.preventDefault();
        listEl?.querySelector<HTMLButtonElement>(".tag-picker__manage button")?.focus();
        return;
      }
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
      document.getElementById(optionID(highlightedIndex))?.scrollIntoView({ block: "nearest" });
      return;
    }
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      if (!open) {
        void show();
        return;
      }
      const option = options[highlightedIndex];
      if (option) void select(option);
    }
  }
</script>

{#snippet optionRow(option: PickerOption, index: number)}
  <AppTooltip text={option.tag ? option.fullName : ""} side="right">
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
      onclick={() => void select(option)}
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
  </AppTooltip>
{/snippet}

<div
  class={["tag-picker", className]}
  bind:this={containerEl}
  onfocusout={onFocusout}
  onfocusin={() => {
    if (searchable && !open && !pending && !quietFocus) void show();
  }}
>
  {#if searchable}
    <TextInput
      value={query || (open ? "" : (selected?.fullName ?? ""))}
      bind:inputEl
      // kit-ui-check-ignore: Typeahead replaces its input with a button on blur; assignment keeps the field visible.
      role="combobox"
      ariaLabel={title}
      ariaExpanded={open && options.length > 0}
      ariaControls={listboxID}
      ariaActivedescendant={open && options.length ? optionID(highlightedIndex) : undefined}
      ariaAutocomplete="list"
      placeholder="Tag name"
      disabled={trulyDisabled}
      block
      oninput={onInput}
      onkeydown={onInputKeydown}
    />
  {:else}
    <AppTooltip text={tooltip ?? `${title}: ${triggerFullName}`} align="start">
      <button
        // kit-ui-check-ignore: pinned kit dropdowns cannot render each inactive tag's color.
        role="combobox"
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
        <ChevronDownIcon class="tag-picker__chevron" size="12" strokeWidth="2" aria-hidden="true" />
      </button>
    </AppTooltip>
  {/if}
  {#if failure}<p class="tag-picker__failure" role="alert">{failure}</p>{/if}

  {#if open && options.length > 0}
    <div class="tag-picker__list kit-popover-card" style={listStyle} bind:this={listEl}>
      <div
        // kit-ui-check-ignore: paired with the scoped color-aware combobox above.
        role="listbox"
        id={listboxID}
      >
        {#if includeAll && options[0]}
          {@render optionRow(options[0], 0)}
        {/if}
        {#each groups as group}
          <div class="tag-picker__group" role="group" aria-label={group.name ?? "Ungrouped tags"}>
            {#if group.name}
              <div class="tag-picker__group-heading">{group.name}</div>
            {/if}
            {#each group.tags as item (item.tag.id)}
              {@const index = optionIndexes.get(item.tag.id) ?? 0}
              {@render optionRow(options[index]!, index)}
            {/each}
          </div>
        {/each}
        {#if canCreate}{@render optionRow(options[options.length - 1]!, options.length - 1)}{/if}
      </div>
      {#if onmanage}
        <div class="tag-picker__manage">
          <button
            type="button"
            class="tag-picker__option"
            disabled={manageDisabled}
            onclick={() => {
              close();
              buttonEl?.focus();
              onmanage?.();
            }}>Manage tags</button
          >
        </div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .tag-picker__failure {
    margin: var(--space-2) 0 0;
    color: var(--accent-red);
    font-size: var(--font-size-xs);
  }

  .tag-picker {
    position: relative;
    min-width: 150px;
  }

  /* The tooltip wrappers take the width of the control they describe. */
  .tag-picker > :global(.kit-tooltip-trigger),
  .tag-picker__list :global(.kit-tooltip-trigger) {
    display: flex;
    width: 100%;
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

  .tag-picker__manage {
    margin-top: var(--space-1);
    padding-top: var(--space-1);
    border-top: 1px solid var(--border-muted);
  }

  .tag-picker__group + .tag-picker__group {
    border-top: 1px solid var(--border-muted);
  }

  .tag-picker__group-heading {
    padding: 6px 8px 3px;
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
    font-weight: var(--font-weight-medium);
    overflow-wrap: anywhere;
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
