import type { Tag } from "./api.js";
import { hashColor } from "@kenn-io/kit-ui";

const NEUTRAL_TAG_COLOR = "#6e7781";

// ColorLabel accepts hex colors only. Keep this palette explicit so the same
// tag identity renders consistently in every theme and component.
const TAG_COLOR_PALETTE = [
  "#0969da",
  "#bf8700",
  "#1a7f37",
  "#cf222e",
  "#8250df",
  "#1b7c83",
  "#bf3989",
  "#4f46e5",
  "#bc4c00",
  "#0576b9",
  "#9a6700",
  "#57606a",
  "#d1242f",
] as const;

export interface PresentedTag {
  tag: Tag;
  group: string | null;
  label: string;
  color: string;
}

export interface TagGroup {
  name: string | null;
  tags: PresentedTag[];
}

export function tagColor(tag: Pick<Tag, "id">): string {
  return tag.id ? hashColor(tag.id, TAG_COLOR_PALETTE) : NEUTRAL_TAG_COLOR;
}

export function splitTagName(name: string): Pick<PresentedTag, "group" | "label"> {
  const segments = name.split("/");
  if (segments.length < 2 || segments.some((segment) => segment === "")) {
    return { group: null, label: name };
  }
  return {
    group: segments.slice(0, -1).join("/"),
    label: segments.at(-1) ?? name,
  };
}

function compareText(left: string, right: string): number {
  return left < right ? -1 : left > right ? 1 : 0;
}

function comparePresentedTags(left: PresentedTag, right: PresentedTag): number {
  return (
    compareText(left.label, right.label) ||
    compareText(left.tag.name, right.tag.name) ||
    compareText(left.tag.id, right.tag.id)
  );
}

export function presentTag(tag: Tag): PresentedTag {
  return { tag, ...splitTagName(tag.name), color: tagColor(tag) };
}

export function groupTags(tags: readonly Tag[]): TagGroup[] {
  const grouped = new Map<string | null, PresentedTag[]>();
  for (const tag of tags) {
    const presented = presentTag(tag);
    const current = grouped.get(presented.group) ?? [];
    current.push(presented);
    grouped.set(presented.group, current);
  }

  return [...grouped.entries()]
    .sort(([left], [right]) => {
      if (left === null) return right === null ? 0 : -1;
      if (right === null) return 1;
      return compareText(left, right);
    })
    .map(([name, items]) => ({
      name,
      tags: [...items].sort(comparePresentedTags),
    }));
}

export function sortTags(tags: readonly Tag[]): Tag[] {
  return groupTags(tags).flatMap((group) => group.tags.map((item) => item.tag));
}
