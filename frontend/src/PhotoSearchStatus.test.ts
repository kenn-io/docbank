import { cleanup, render, screen } from "@testing-library/svelte";
import { afterEach, expect, it } from "vite-plus/test";
import PhotoSearchStatus from "./PhotoSearchStatus.svelte";

afterEach(cleanup);

it.each([
  [
    "unsupported_query",
    false,
    "This query can't run as an image search. Showing matches based on photo details.",
  ],
  [
    "consent_required",
    false,
    "Image search needs consent. Showing matches based on photo details.",
  ],
  [
    "provider_unavailable",
    false,
    "Image search is unavailable. Showing matches based on photo details.",
  ],
  [
    "similarity_floor_unset",
    false,
    "Image search isn't set up. Showing matches based on photo details.",
  ],
  ["", true, "Some photos aren't indexed yet. Showing available image matches."],
] as const)("explains image search status: %s incomplete: %s", (reason, incomplete, message) => {
  render(PhotoSearchStatus, {
    status: { fallback_reason: reason, coverage_incomplete: incomplete },
    total: 3,
  });
  expect(screen.getByRole("status").textContent?.replace(/\s+/g, " ")).toContain(message);
});

it.each([
  [true, 3, "Top 3 located matches. Narrow the search to see others."],
  [false, 12, "Top 12 matches. Select all, export, and Cull use these photos."],
] as const)("shows the search limit for located=%s", (located, total, message) => {
  render(PhotoSearchStatus, { status: { truncated: true }, total, located });
  expect(screen.getByRole("status").textContent?.replace(/\s+/g, " ")).toContain(message);
});
