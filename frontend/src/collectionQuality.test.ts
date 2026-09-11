import { afterEach, describe, expect, it, vi } from "vitest";
import { collectionQuality, qualitySuggestion } from "./collectionQuality.js";

const id = "11111111-1111-4111-8111-111111111111";
const coverage = {configuration:"configured",profile:"archive",profiles:["archive"],profile_fingerprint:"a".repeat(64),generation_id:"",counts:{complete:1,partial:0,failed:0,unprocessed:1,none:0}};
const receipt = () => ({collection:{id,source_kind:"cli",source_description:"Synthetic import",started_at:"2026-09-11T00:00:00Z",file_count:2,total_bytes:20,label:null,label_revision:1,label_updated_at:"2026-09-11T00:00:00Z",coverage:structuredClone(coverage)},source_fingerprint:"b".repeat(64),dimensions:[{field:"extension",values:[{value:"pdf",count:1},{value:"txt",count:1}],missing:0,other:0}],zero_bytes:0,mismatches:0,duplicate_documents:0,spikes:[]});
afterEach(()=>vi.restoreAllMocks());

describe("collection quality receipts",()=>{
  it("binds the selected collection, profile, session and cancellation",async()=>{
    const controller=new AbortController();
    const fetch=vi.spyOn(globalThis,"fetch").mockResolvedValue(new Response(JSON.stringify(receipt())));
    const result=await collectionQuality("session",id,"archive",controller.signal);
    expect(result.collection.coverage.counts?.complete).toBe(1);
    const [url,options]=fetch.mock.calls[0];
    expect(String(url)).toBe(`/api/v1/collections/${id}/quality?profile=archive`);
    expect(new Headers(options?.headers).get("X-Docbank-Web-Session")).toBe("session");
    expect(options?.signal).toBe(controller.signal);
  });
  for(const mutation of ["identity","profile","counts","sum","dimension","buckets","fingerprint"]){
    it(`rejects a malformed ${mutation} receipt`,async()=>{
      const value=receipt();
      if(mutation==="identity")value.collection.id="22222222-2222-4222-8222-222222222222";
      if(mutation==="profile")value.collection.coverage.profile="different";
      if(mutation==="counts")Object.assign(value.collection.coverage,{counts:null});
      if(mutation==="sum")value.collection.coverage.counts.complete=4;
      if(mutation==="dimension")value.dimensions[0].field="invented";
      if(mutation==="buckets")value.dimensions[0].values[0].count=-1;
      if(mutation==="fingerprint")value.source_fingerprint="not-a-digest";
      vi.spyOn(globalThis,"fetch").mockResolvedValue(new Response(JSON.stringify(value)));
      await expect(collectionQuality("session",id,"archive",new AbortController().signal)).rejects.toThrow();
    });
  }
  it("keeps unavailable configuration distinct from empty output",async()=>{
    const value=receipt();
    Object.assign(value.collection.coverage,{configuration:"unconfigured",profile:"",profiles:[],profile_fingerprint:"",counts:null});
    vi.spyOn(globalThis,"fetch").mockResolvedValue(new Response(JSON.stringify(value)));
    expect((await collectionQuality("session",id,"",new AbortController().signal)).collection.coverage.configuration).toBe("unconfigured");
  });
  it("creates an explicit OR expression scoped to the collection",()=>{
    expect(qualitySuggestion(id,"extension",["pdf","txt"])).toEqual({v:1,text:'(extension:"pdf" OR extension:"txt")',syntax:"advanced",mode:"lexical",filters:{collection_ids:[id]},sort:{field:"name",direction:"asc"}});
    expect(()=>qualitySuggestion(id,"extension",[])).toThrow();
    expect(()=>qualitySuggestion(id,"modified_month",["2026-09"])).toThrow();
	  expect(()=>qualitySuggestion(id,"duplicates",["unknown"])).toThrow();
  });
});
