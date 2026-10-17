<script lang="ts">
  import { Button, Checkbox, SelectDropdown, Spinner, Notice } from "@kenn-io/kit-ui";
  import TagPicker from "./TagPicker.svelte";
  import type { PhotoAuthoredEditor } from "./photoAuthored.svelte.js";
  import type {
    PhotoAuthoredPatch,
    PhotoKeywordEdit,
    PhotoAuthoredReceipt,
    Tag,
  } from "./generated/docbank.js";
  let {
    editor,
    count = 1,
    actionDisabled = false,
    onedit,
    onresponse,
  }: {
    editor: PhotoAuthoredEditor;
    count?: number;
    actionDisabled?: boolean;
    onresponse?: (receipt?: PhotoAuthoredReceipt, tags?: Tag[]) => Promise<void>;
    onedit: (patch: PhotoAuthoredPatch, keywords?: PhotoKeywordEdit[]) => void;
  } = $props();
  let keyword = $state("");
  let selections = $state<Record<string, { caption: boolean; tags: string[] }>>({});
  function toggleSuggestion(id: string, tag?: string) {
    const selected = selections[id] ?? { caption: false, tags: [] };
    selections[id] =
      tag === undefined
        ? { ...selected, caption: !selected.caption }
        : {
            ...selected,
            tags: selected.tags.includes(tag)
              ? selected.tags.filter((value) => value !== tag)
              : [...selected.tags, tag],
          };
  }
  const disabled = $derived(
    actionDisabled ||
      editor.concealed ||
      editor.conflict ||
      editor.busy ||
      editor.loading ||
      (!!editor.error && !editor.targets.length),
  );
  const fields = ["caption", "creator", "copyright"] as const;
  const choices = [
    {
      field: "rating",
      label: "Rating",
      options: [0, 1, 2, 3, 4, 5].map((value) => ({
        value: String(value),
        label: value ? `${value} star${value === 1 ? "" : "s"}` : "Unrated",
      })),
    },
    {
      field: "flag",
      label: "Flag",
      options: [
        { value: "", label: "Unflagged" },
        { value: "pick", label: "Pick" },
        { value: "reject", label: "Reject" },
      ],
    },
    {
      field: "label",
      label: "Color label",
      options: ["", "red", "yellow", "green", "blue", "purple"].map((value) => ({
        value,
        label: value ? value[0]!.toUpperCase() + value.slice(1) : "No label",
      })),
    },
    {
      field: "rotation",
      label: "Rotation",
      options: [0, 90, 180, 270].map((value) => ({ value: String(value), label: `${value}°` })),
    },
  ] as const;
  function select(field: "rating" | "flag" | "label" | "rotation", value: string) {
    if (value === "mixed") return;
    editor.stage({ [field]: field === "rating" || field === "rotation" ? Number(value) : value });
  }
</script>

<section class="photo-inspector" aria-label="Photo metadata">
  <h2>{count === 1 ? "Metadata" : `${count.toLocaleString()} photos`}</h2>
  {#if editor.concealed}{#if editor.validating}<span role="status"
        ><Spinner size={14} />Loading metadata…</span
      >{:else}<span role="status">Metadata unavailable</span>{/if}{:else}
    {#if editor.loading}<span role="status"><Spinner size={14} />Loading metadata…</span>{/if}
    {#if editor.asset && editor.asset.files.filter((file) => file.role !== "sidecar").length > 1}
      <SelectDropdown
        title="Edit members"
        value={editor.fileID}
        options={[
          { value: "", label: "Whole pair" },
          ...editor.asset.files
            .filter((file) => file.role !== "sidecar")
            .map((file) => ({
              value: file.id,
              label: `${file.role.toUpperCase()} · ${editor.targets.find((target) => target.file_id === file.id)?.file_name ?? file.id}`,
            })),
        ]}
        disabled={disabled || editor.dirty}
        onchange={(value) => editor.member(value)}
      />
    {/if}
    {#each choices as choice}
      {@const value = editor.value(choice.field)}
      <div class="field">
        <span>{choice.label}</span><SelectDropdown
          title={choice.label}
          value={value === undefined ? "mixed" : String(value)}
          options={[
            ...(value === undefined ? [{ value: "mixed", label: "Mixed" }] : []),
            ...choice.options,
          ]}
          {disabled}
          onchange={(value) => select(choice.field, value)}
        />
      </div>
    {/each}
    {#each fields as field}
      {@const value = editor.value(field)}
      <div class="field">
        <span>{field[0]!.toUpperCase() + field.slice(1)}</span><textarea
          aria-label={field[0]!.toUpperCase() + field.slice(1)}
          value={String(value ?? "")}
          placeholder={value === undefined ? "Mixed" : ""}
          {disabled}
          rows="2"
          oninput={(event) => editor.stage({ [field]: event.currentTarget.value })}
        ></textarea>{#if value === undefined}<Button
            size="sm"
            {disabled}
            onclick={() => editor.stage({ [field]: "" })}>Clear {field}</Button
          >{/if}
      </div>
    {/each}
    {#if editor.tagsError}<Notice tone="error" message={editor.tagsError} /><Button
        size="sm"
        disabled={editor.tagsLoading}
        onclick={() => void editor.loadTags?.()}>Retry keywords</Button
      >{/if}
    {#if editor.tagsTotal > editor.tags.length}<Button
        size="sm"
        disabled={editor.tagsLoading}
        onclick={() => void editor.loadMoreTags?.()}>Load more keywords</Button
      >{/if}
    <div class="field">
      <span>Keywords</span><TagPicker
        value={keyword}
        tags={editor.tags}
        title="Keyword"
        placeholder="Choose keyword"
        disabled={disabled || editor.tagsLoading}
        onchange={(value) => (keyword = value)}
      />
    </div>
    {#if keyword}<div class="keyword-actions">
        <Button
          size="sm"
          {disabled}
          onclick={() => editor.stage({}, [{ tag_id: keyword, assign: true }])}>Add keyword</Button
        ><Button
          size="sm"
          {disabled}
          onclick={() => editor.stage({}, [{ tag_id: keyword, assign: false }])}
          >Remove keyword</Button
        >{#if editor.keywordState(keyword)}<span
            >{editor.keywordState(keyword) === "mixed" ? "Mixed" : "Assigned"}</span
          >{/if}
      </div>{/if}
    {#if editor.files.length}<ul aria-label="Assigned keywords">
        {#each editor.tags.filter((tag) => editor.keywordState(tag.id)) as tag}<li>
            {tag.name}{editor.keywordState(tag.id) === "mixed" ? " · Mixed" : ""}
          </li>{/each}
      </ul>{/if}
    {#if editor.conflict && (!editor.membershipChanged || !editor.targets.length)}<Notice
        tone="warning"
        message={editor.targets.length
          ? "Review the current photos before applying these edits."
          : "No photos are available to review. Clear edits to discard this draft."}
      />{/if}
    {#if editor.membershipChanged && editor.targets.length}
      <Notice
        tone="warning"
        message="Photo membership changed. These remaining files will receive your edits."
      />
      <ul aria-label="Members to review">
        {#each editor.files as target}<li>{target.file_name}</li>{/each}
      </ul>
      <Button disabled={editor.busy || editor.loading} onclick={() => editor.reviewMembers()}
        >Use reviewed members</Button
      >
    {:else if editor.conflict && editor.targets.length}<Button
        disabled={editor.busy || editor.loading}
        onclick={() => void editor.reload()}>Reload metadata</Button
      >{/if}
    {#if count === 1}
      {#if editor.activeSuggestion}
        {@const suggestion = editor.activeSuggestion}
        {@const choices = editor.suggestionChoices(suggestion)}
        {@const selected = selections[suggestion.suggestion_id]}
        {@const caption = choices.caption && !!selected?.caption}
        {@const tags = choices.tags.filter((tag) => selected?.tags.includes(tag))}
        {#if choices.caption || choices.tags.length || suggestion.value.visible_text}
          <section class="suggestions" aria-label="Suggested photo metadata">
            <h3>Suggested</h3>
            {#if choices.caption}
              <Checkbox
                label={suggestion.value.caption}
                checked={caption}
                disabled={disabled || editor.dirty}
                onchange={() => toggleSuggestion(suggestion.suggestion_id)}
              />
            {/if}
            <div class="suggested-tags">
              {#each choices.tags as tag (tag)}
                <Checkbox
                  label={tag}
                  checked={tags.includes(tag)}
                  disabled={disabled || editor.dirty}
                  onchange={() => toggleSuggestion(suggestion.suggestion_id, tag)}
                />
              {/each}
            </div>
            {#if choices.caption || choices.tags.length}
              <Button
                size="sm"
                disabled={disabled || editor.dirty || (!caption && !tags.length)}
                onclick={() =>
                  void editor.respondSuggestion(suggestion, caption, tags, false, onresponse)}
                >Accept selected</Button
              >
            {/if}
            {#if suggestion.value.visible_text}<h4>Visible text</h4>
              <p>{suggestion.value.visible_text}</p>{/if}
            <Button
              size="sm"
              disabled={disabled || editor.dirty}
              onclick={() => void editor.respondSuggestion(suggestion, false, [], true, onresponse)}
              >Dismiss</Button
            >
          </section>
        {/if}
      {/if}
    {/if}
    {#if editor.suggestionsError}<Notice tone="warning" message={editor.suggestionsError} />{/if}
    <Button disabled={disabled || editor.conflict || !editor.dirty} onclick={() => onedit({})}
      >Apply</Button
    >
  {/if}
  {#if editor.dirty}<Button
      disabled={editor.busy || editor.loading}
      onclick={() => editor.clearDraft()}>Clear edits</Button
    >{/if}
</section>

<style>
  .suggestions {
    border-top: 1px solid var(--border-default);
    padding-top: var(--space-3);
  }
  .suggested-tags {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    margin-block: var(--space-2);
  }
  .photo-inspector {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  h2 {
    margin: 0;
    font-size: var(--font-size-sm);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: 4px;
    font-size: var(--font-size-xs);
  }
  .field > span {
    color: var(--text-muted);
  }
  textarea {
    width: 100%;
    box-sizing: border-box;
    resize: vertical;
    padding: var(--space-3);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-md);
    background: var(--bg-surface);
    color: var(--text-primary);
    font: inherit;
  }
  .keyword-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    align-items: center;
    font-size: var(--font-size-xs);
  }
  ul {
    margin: 0;
    padding-left: 16px;
    font-size: var(--font-size-xs);
  }
</style>
