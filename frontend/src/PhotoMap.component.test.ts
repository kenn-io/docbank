import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/svelte";
import { tick } from "svelte";
import { mapPhoto } from "./photo-test-fixtures.js";
const mocks = vi.hoisted(() => {
 const frames = new Map<number,()=>void>();let serial=0;
 const util={requestAnimFrame:vi.fn((callback:()=>void)=>{frames.set(++serial,callback);return serial;}),cancelAnimFrame:vi.fn((id:number)=>frames.delete(id))};
 const bounds = {latitude:2};
 const map = {fitBounds:vi.fn(), project:vi.fn((position: number[])=>({lat:position[0],lng:position[1]})), getPixelBounds:vi.fn(()=>({contains:(position: {lat:number}) => position.lat < bounds.latitude})), getZoom:()=>0, getCenter:()=>({lat:10,lng:20}), getMaxZoom:()=>0, on:vi.fn(), invalidateSize:vi.fn(), remove:vi.fn(), setView:vi.fn()};
 map.setView.mockReturnValue(map);
 const cluster = {addLayers:vi.fn(), clearLayers:vi.fn(), removeLayers:vi.fn(), getBounds:vi.fn(), on:vi.fn(), addTo:vi.fn(), getVisibleParent:vi.fn()};
 const marker = vi.fn((coordinates: number[]) => ({getLatLng:() => ({lat:coordinates[0], lng:coordinates[1], equals:(value:number[]) => value[0] === coordinates[0] && value[1] === coordinates[1]}), setLatLng:vi.fn((value:number[]) => {coordinates=value;}), options:{title:"Photo "+coordinates[0]+".jpg"}, getElement:()=>undefined, on:vi.fn()}));
 const tile = {addTo:vi.fn(),on:vi.fn()}; tile.on.mockReturnValue(tile);
 return {frames,util,bounds, map, cluster, marker, makeMap:vi.fn(() => map), tileLayer:vi.fn(() => tile), tile};
});
vi.mock("leaflet", () => ({default:{Util:mocks.util,map:mocks.makeMap, marker:mocks.marker, tileLayer:mocks.tileLayer, markerClusterGroup:()=>mocks.cluster, divIcon:vi.fn()}}));
vi.mock("leaflet.markercluster", () => ({}));
import type { PhotoMapRow } from "./generated/docbank.js";
import PhotoMap from "./PhotoMap.svelte";
afterEach(() => { cleanup(); mocks.frames.clear();vi.clearAllMocks(); mocks.bounds.latitude=2; vi.unstubAllGlobals(); });
function frame() {const [id,callback]=mocks.frames.entries().next().value!;mocks.frames.delete(id);callback();}

function setup(items: PhotoMapRow[] = [{...mapPhoto(1),latitude:1,longitude:1}]) {
 vi.stubGlobal("ResizeObserver", class {observe() {} disconnect() {}});
 const props = {items, tiles:{tile_url:"https://tiles.example.org/{z}/{x}/{y}", attribution:"Example", max_zoom:0}, onopen:vi.fn(), onvisible:vi.fn()};
 return {props, view:render(PhotoMap, props)};
}
it("renders located rows and cancels pending batches on replacement and disposal", async () => {
 const first={...mapPhoto(1),latitude:1,longitude:1},second={...mapPhoto(2),latitude:2,longitude:2};const {props,view}=setup([...Array.from({length:201},(_,id)=>({...mapPhoto(id),latitude:id % 90,longitude:id % 180})),{...mapPhoto(202),latitude:null,longitude:null}]);
 frame();expect(mocks.marker).toHaveBeenCalledTimes(200);expect(mocks.frames.size).toBe(1);
 await view.rerender({...props,items:[second]});
 expect(mocks.cluster.removeLayers).toHaveBeenLastCalledWith(expect.arrayContaining([expect.anything()]));expect(mocks.marker).toHaveBeenCalledTimes(200);expect(props.onvisible).toHaveBeenLastCalledWith([]);
 await view.rerender({...props,items:[first,{...mapPhoto(3),latitude:null,longitude:null}]});frame();expect(mocks.cluster.addLayers.mock.calls.at(-1)![0]).toHaveLength(1);
 await view.rerender({...props,items:Array.from({length:201},(_,id)=>({...mapPhoto(id),latitude:id % 90,longitude:id % 180}))});frame();
 view.unmount();expect(mocks.frames.size).toBe(0);expect(mocks.util.cancelAnimFrame).toHaveBeenCalled();expect(mocks.map.remove).toHaveBeenCalledOnce();
});

it("recomputes visible rows when the map moves", () => {
 const second={...mapPhoto(2),latitude:2,longitude:2};const {props}=setup([second]);
 mocks.bounds.latitude=3;
 const moved = mocks.map.on.mock.calls.find(([name]) => name === "moveend")![1]; moved();
 expect(props.onvisible).toHaveBeenLastCalledWith([second]);
});
it("restores and reports the map view", () => {
 vi.stubGlobal("ResizeObserver", class {observe() {} disconnect() {}});
 const onview=vi.fn();
 render(PhotoMap,{items:[{...mapPhoto(1),latitude:1,longitude:1}], tiles:{tile_url:"https://tiles.example.org/{z}/{x}/{y}", attribution:"Example", max_zoom:0}, view:{lat:5,lng:6,zoom:7}, onopen:vi.fn(), onvisible:vi.fn(), onview});
 expect(mocks.map.setView).toHaveBeenCalledWith([5,6],7);
 const moved = mocks.map.on.mock.calls.find(([name]) => name === "moveend")![1]; moved();
 expect(onview).toHaveBeenCalledWith({lat:10,lng:20,zoom:0});
 onview.mockClear();cleanup();expect(()=>moved()).not.toThrow();expect(onview).not.toHaveBeenCalled();
});
it("restores focus to a visible parent or the map without moving it", () => {
 const second={...mapPhoto(2),latitude:2,longitude:2};const {view}=setup([second]);frame();const child=mocks.marker.mock.results[0]!.value;
 const focus=vi.fn(); mocks.cluster.getVisibleParent.mockReturnValue({getElement:()=>({focus})});
 view.component.focus(second.asset_id);
 expect(mocks.cluster.getVisibleParent).toHaveBeenCalledWith(child); expect(focus).toHaveBeenCalledWith({preventScroll:true});
 mocks.cluster.getVisibleParent.mockReturnValue(undefined);
 const container=view.container.querySelector<HTMLElement>(".photo-map")!; const fallback=vi.spyOn(container,"focus");view.component.focus(second.asset_id);
 expect(fallback).toHaveBeenCalledWith({preventScroll:true});expect(mocks.map.setView).toHaveBeenCalledTimes(1);expect(mocks.map.fitBounds).not.toHaveBeenCalled();
});
it("bounds the map and tiles to one world", () => {
 const {props}=setup();
 expect(mocks.makeMap).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({maxBounds:[[-90,-180],[90,180]],maxBoundsViscosity:1}));
 expect(mocks.tileLayer).toHaveBeenCalledWith(props.tiles.tile_url, expect.objectContaining({maxZoom:0,noWrap:true}));
});

it("shows one provider notice when tiles fail repeatedly", async () => {
 setup();
 const failed = mocks.tile.on.mock.calls.find(([name]) => name === "tileerror")![1];
 failed(); failed(); await tick();
 const notices = screen.getAllByRole("alert");
 expect(notices).toHaveLength(1);
 expect(notices[0].textContent).toBe("Map tiles from tiles.example.org couldn't load. Check [web.map_tiles], then restart Docbank.");
});

it("replaces an interior marker when its coordinates or name change", async () => {
 const first={...mapPhoto(1),latitude:1,longitude:1},second={...mapPhoto(2),latitude:2,longitude:2};const {view}=setup([first,second]);frame();
 await view.rerender({items:[{...first,latitude:4,name:"Moved photo.jpg"},second]});frame();
 expect(mocks.marker).toHaveBeenLastCalledWith([4,1],expect.objectContaining({title:"Moved photo.jpg"}));expect(mocks.cluster.removeLayers.mock.calls.at(-1)![0]).toHaveLength(1);
});
