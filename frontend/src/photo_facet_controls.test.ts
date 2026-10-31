import { afterEach, expect, it, vi } from "vite-plus/test";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/svelte";
import FacetSidebar from "./FacetSidebar.svelte";
import type { Query } from "./query.js";

afterEach(cleanup);

it.each([
  {
    facet: "Location",
    dimension: "location" as const,
    key: "Paris",
    filters: { locations: ["Paris"] },
    selected: "Paris",
    clears: ["locations"],
  },
  {
    facet: "Albums",
    dimension: "set" as const,
    key: "11111111-1111-4111-8111-111111111111",
    filters: { set_ids: ["11111111-1111-4111-8111-111111111111"] },
    selected: "11111111-1111-4111-8111-111111111111",
    clears: ["set_ids"],
  },
  {
    facet: "Rating",
    dimension: "rating" as const,
    key: "3",
    filters: { rating_min: 3, rating_max: 3 },
    selected: "3 stars",
    clears: ["rating_min", "rating_max"],
  },
  {
    facet: "Flag",
    dimension: "flag" as const,
    key: "pick",
    filters: { flags: ["pick" as const] },
    selected: "pick",
    clears: ["flags"],
  },
  {
    facet: "Color label",
    dimension: "label" as const,
    key: "red",
    filters: { labels: ["red" as const] },
    selected: "red",
    clears: ["labels"],
  },
])(
  "keeps the applied $facet editable after switching to Meaning",
  async ({ facet, dimension, key, filters, selected, clears }) => {
    const onchange = vi.fn();
    const query: Query = {
      v: 1,
      text: "sunset",
      syntax: "simple",
      mode: "lexical",
      filters,
      sort: { field: "name", direction: "asc" },
    };
    const view = render(FacetSidebar, {
      unit: "photos",
      query,
      onchange,
      facets: [
        {
          dimension,
          available: true,
          total: 1,
          missing: 0,
          other: 0,
          values: [{ key, label: selected, count: 1, selected: true }],
        },
      ],
    });
    expect(screen.getByRole("group", { name: `${facet} facet` })).toBeTruthy();
    await view.rerender({
      query: { ...query, mode: "semantic", sort: { field: "relevance", direction: "desc" } },
      facets: [],
    });
    const button = within(screen.getByRole("group", { name: `${facet} facet` })).getByRole(
      "button",
      { name: `${selected}, selected` },
    );
    expect(button.getAttribute("aria-pressed")).toBe("true");
    await fireEvent.click(button);
    for (const field of clears) expect(onchange.mock.lastCall?.[0].filters[field]).toBeUndefined();
  },
);
