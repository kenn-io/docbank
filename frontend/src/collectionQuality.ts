import { requestJSON } from "./api.js";
import { readCollection, type Collection } from "./collections.js";
import { parseQuery, type Query } from "./query.js";

export interface CoverageCounts { complete:number; partial:number; failed:number; unprocessed:number; none:number }
export interface ProcessingCoverage {
  configuration:"configured"|"unconfigured"|"profile_required";
  profile:string; profiles:string[]; profile_fingerprint:string; generation_id:string;
  counts:CoverageCounts|null;
}
export interface QualityDimension { field:string; values:{value:string;count:number}[]; missing:number; other:number }
export interface CollectionQuality {
  collection:Collection & {coverage:ProcessingCoverage}; source_fingerprint:string;
  dimensions:QualityDimension[]; zero_bytes:number; mismatches:number;
  duplicate_documents:number; spikes:{field:string;value:string;count:number}[];
}
const fields=new Set(["extension","media_type","media_family","modified_month","size","text_coverage","duplicates"]);
const states=["complete","partial","failed","unprocessed","none"] as const;
const uuid=/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const digest=/^[0-9a-f]{64}$/;
function check(value:unknown):asserts value {if(!value)throw new Error("Invalid collection quality receipt. Reload before continuing.");}
function object(value:unknown):Record<string,unknown>{check(value!==null&&typeof value==="object"&&!Array.isArray(value));return value as Record<string,unknown>;}
function count(value:unknown,total:number):value is number{return typeof value==="number"&&Number.isSafeInteger(value)&&value>=0&&value<=total;}

export function readCoverage(value:unknown,total:number):ProcessingCoverage {
  const raw=object(value);
  check(["configured","unconfigured","profile_required"].includes(String(raw.configuration)));
  check(typeof raw.configuration==="string"&&typeof raw.profile==="string"&&raw.profile.length<=128);
  check(Array.isArray(raw.profiles)&&raw.profiles.length<=64&&raw.profiles.every(x=>typeof x==="string"&&x.length>0&&x.length<=128));
  check(new Set(raw.profiles).size===raw.profiles.length);
  check(typeof raw.profile_fingerprint==="string"&&typeof raw.generation_id==="string"&&raw.generation_id.length<=1024);
  if(raw.configuration==="configured"){
    check(raw.profiles.includes(raw.profile)&&digest.test(raw.profile_fingerprint));
    const counts=object(raw.counts);
    check(Object.keys(counts).length===states.length&&states.every(key=>count(counts[key],total)));
    check(states.reduce((sum,key)=>sum+(counts[key] as number),0)===total);
  }else check(raw.counts===null&&raw.profile===""&&raw.profile_fingerprint==="");
  return raw as unknown as ProcessingCoverage;
}

export async function collectionQuality(session:string,id:string,profile:string,signal:AbortSignal):Promise<CollectionQuality>{
  check(uuid.test(id));
  const params=new URLSearchParams();if(profile)params.set("profile",profile);
  const suffix=params.size?`?${params}`:"";
  const raw=object(await requestJSON<unknown>(`/api/v1/collections/${id}/quality${suffix}`,session,{signal}));
  const collection=readCollection(raw.collection);
  check(collection.id===id);
  const coverage=readCoverage(object(raw.collection).coverage,collection.file_count);
  if(profile)check(coverage.profile===profile);
  check(typeof raw.source_fingerprint==="string"&&digest.test(raw.source_fingerprint));
  check(Array.isArray(raw.dimensions)&&raw.dimensions.length<=7);
  const seen=new Set<string>();
  for(const value of raw.dimensions){
    const d=object(value);check(typeof d.field==="string"&&fields.has(d.field)&&!seen.has(d.field));seen.add(d.field);
    check(Array.isArray(d.values)&&d.values.length<=50&&count(d.missing,collection.file_count)&&count(d.other,collection.file_count));
    let total=d.missing+d.other;const keys=new Set<string>();
    for(const bucket of d.values){const b=object(bucket);check(typeof b.value==="string"&&b.value.length<=1024&&!keys.has(b.value)&&count(b.count,collection.file_count));keys.add(b.value);total+=b.count;}
    check(total===collection.file_count);
  }
  for(const key of ["zero_bytes","mismatches","duplicate_documents"])check(count(raw[key],collection.file_count));
  check(Array.isArray(raw.spikes)&&raw.spikes.length<=7);
  for(const spike of raw.spikes){const s=object(spike);check(typeof s.field==="string"&&fields.has(s.field)&&typeof s.value==="string"&&s.value.length<=1024&&count(s.count,collection.file_count));}
  return {...raw,collection:{...collection,coverage}} as unknown as CollectionQuality;
}

export function qualitySuggestion(id:string,field:string,values:string[]):Query {
  check(uuid.test(id)&&["extension","media_type","media_family","text_coverage","duplicates"].includes(field));
  check(values.length>0&&values.length<=32&&values.every(value=>value.length>0&&value.length<=1024));
  if(field==="duplicates")check(values.every(value=>value==="duplicate"||value==="unique"));
  const operand=field==="media_type"?"mime":field==="duplicates"?"has_duplicates":field;
  const terms=values.map(value=>`${operand}:${JSON.stringify(field==="duplicates"?(value==="duplicate"?"true":"false"):value)}`);
  return parseQuery(JSON.stringify({v:1,text:`(${terms.join(" OR ")})`,syntax:"advanced",mode:"lexical",filters:{collection_ids:[id]},sort:{field:"name",direction:"asc"}}));
}
