export interface MapTiles {
  tile_url: string;
  host: string;
  attribution: string;
  max_zoom: number;
}
export function mapTiles(): MapTiles {
  const value = document.querySelector<HTMLMetaElement>('meta[name="docbank-map-tiles"]')?.content;
  return value ? JSON.parse(value) : { tile_url: "", host: "", attribution: "", max_zoom: 19 };
}
