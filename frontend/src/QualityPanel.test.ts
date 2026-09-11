import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/svelte";
import QualityPanel from "./QualityPanel.svelte";
const id="11111111-1111-4111-8111-111111111111";
function receipt(){return {collection:{id,source_kind:"cli",source_description:"Synthetic import",started_at:"2026-09-11T00:00:00Z",file_count:5,total_bytes:20,label:null,label_revision:1,label_updated_at:"2026-09-11T00:00:00Z",coverage:{configuration:"configured",profile:"archive",profiles:["archive"],profile_fingerprint:"a".repeat(64),generation_id:"",counts:{complete:1,partial:1,failed:1,unprocessed:1,none:1}}},source_fingerprint:"b".repeat(64),dimensions:[{field:"extension",values:[{value:"pdf",count:3},{value:"txt",count:2}],missing:0,other:0}],zero_bytes:1,mismatches:0,duplicate_documents:0,spikes:[]};}
const response=(value:unknown)=>new Response(JSON.stringify(value));
const props=()=>({session:"session",collectionID:id,profile:"",onprofile:vi.fn(),onnewquery:vi.fn(),onauthfailure:vi.fn()});
afterEach(()=>{cleanup();vi.restoreAllMocks();});
it("shows all states and opens a selected OR suggestion only on action",async()=>{
  const fetch=vi.spyOn(globalThis,"fetch").mockResolvedValue(response(receipt()));
  const p=props();render(QualityPanel,p);
  await screen.findByText("Complete: 1");
  for(const label of ["Partial: 1","Failed: 1","Unprocessed: 1","No text: 1"])expect(screen.getByText(label)).toBeTruthy();
  await fireEvent.click(screen.getByLabelText("Include extension pdf"));
  await fireEvent.click(screen.getByLabelText("Include extension txt"));
  expect(p.onnewquery).not.toHaveBeenCalled();
  await fireEvent.click(screen.getByRole("button",{name:"New query for selected extension"}));
  expect(p.onnewquery.mock.calls[0][0].text).toBe('(extension:"pdf" OR extension:"txt")');
  expect(fetch.mock.calls.every(([url])=>String(url).includes("/quality"))).toBe(true);
});
it("shows missing configuration without inventing zero counts",async()=>{
  const value=receipt();Object.assign(value.collection.coverage,{configuration:"unconfigured",profile:"",profiles:[],profile_fingerprint:"",counts:null});
  vi.spyOn(globalThis,"fetch").mockResolvedValue(response(value));render(QualityPanel,props());
  await screen.findByText("Processing is not configured.");
  expect(screen.queryByText("Complete: 0")).toBeNull();
});
it("aborts old requests and ignores their result after collection change",async()=>{
  let resolve!:(value:Response)=>void;const old=new Promise<Response>(r=>{resolve=r;});
  const fetch=vi.spyOn(globalThis,"fetch").mockReturnValueOnce(old).mockResolvedValue(response({...receipt(),collection:{...receipt().collection,id:"22222222-2222-4222-8222-222222222222"}}));
  const view=render(QualityPanel,props());
  await waitFor(()=>expect(fetch).toHaveBeenCalledTimes(1));
  const signal=fetch.mock.calls[0][1]?.signal;
  await view.rerender({collectionID:"22222222-2222-4222-8222-222222222222"});
  await screen.findByText("Complete: 1");expect(signal?.aborted).toBe(true);
  resolve(response({broken:true}));await Promise.resolve();
  expect(screen.queryByRole("alert")).toBeNull();
  view.unmount();expect(fetch.mock.calls[1][1]?.signal?.aborted).toBe(true);
});
