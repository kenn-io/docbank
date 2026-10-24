import { cleanup, render, screen } from "@testing-library/svelte";
import { afterEach, expect, it } from "vite-plus/test";
import PhotoSearchStatus from "./PhotoSearchStatus.svelte";

afterEach(cleanup);

it.each([
  [
    "unsupported_query",
    false,
    "No searchable text or unsupported syntax. Showing metadata matches.",
  ],
  ["consent_required", false, "Image search needs consent. Showing metadata matches."],
  ["provider_unavailable", false, "Image search is unavailable. Showing metadata matches."],
  ["", true, "Some photos aren't indexed yet. Showing available image matches."],
] as const)("explains image search status: %s incomplete: %s", (reason, incomplete, message) => {
  render(PhotoSearchStatus, {
    status: { fallback_reason: reason, coverage_incomplete: incomplete },
    total: 3,
  });
  expect(screen.getByRole("status").textContent).toContain(message);
});

it.each([
  [true, 3, "Top 3 located matches. Narrow the search to see others."],
  [false, 12, "Top 12 matches. Counts, Select all, export, and Cull use these photos."],
] as const)("shows the search limit for located=%s", (located, total, message) => {
  render(PhotoSearchStatus, { status: { truncated: true }, total, located });
  expect(screen.getByRole("status").textContent).toContain(message);
});
