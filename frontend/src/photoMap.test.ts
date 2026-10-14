import { afterEach, expect, it } from "vitest";
import { mapTiles } from "./photoMap.js";

afterEach(() => document.querySelectorAll('meta[name="docbank-map-tiles"]').forEach(meta => meta.remove()));

it("reads the same-origin tile template, provider host, attribution and max zoom", () => {
  const tiles = { tile_url: "/api/daemon/web-map-tiles/{z}/{x}/{y}", host: "tiles.example.org", attribution: "Example provider", max_zoom: 17 };
  const meta = document.createElement("meta");
  meta.name = "docbank-map-tiles";
  meta.content = JSON.stringify(tiles);
  document.head.append(meta);
  expect(mapTiles()).toEqual(tiles);
});

it("keeps maps disabled without an embedded provider", () => {
  expect(mapTiles()).toEqual({ tile_url: "", host: "", attribution: "", max_zoom: 19 });
});
