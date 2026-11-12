package mcp

import "maps"

const contractSafeInteger = 9_007_199_254_740_991

func contractDateSchema(nullable bool) schema {
	d := schema{"type": "string", "format": "date", "pattern": "^[0-9]{4}-[0-9]{2}-[0-9]{2}$"}
	if nullable {
		return schema{"anyOf": []schema{d, {"type": "null"}}}
	}
	return d
}
func contractEditableProperties() schema {
	p := schema{"title": schema{"type": "string", "minLength": 1, "maxLength": 300}, "contract_type": schema{"type": "string", "pattern": "^([a-z0-9_]{1,64})?$"}, "status": enumSchema("draft", "pending_signature", "active", "terminated", "cancelled")}
	for _, k := range []string{"starts_on", "expires_on", "signed_on"} {
		p[k] = contractDateSchema(true)
	}
	for _, k := range []string{"summary", "purpose", "consideration", "termination"} {
		p[k] = stringSchema(8192)
	}
	return p
}
func contractListItemProperties() schema {
	return schema{"contract_id": uuidSchema(), "title": stringSchema(300), "contract_type": stringSchema(64), "status": stringSchema(32), "effective_status": stringSchema(32), "starts_on": contractDateSchema(true), "expires_on": contractDateSchema(true), "signed_on": contractDateSchema(true), "revision": integerSchema(1, contractSafeInteger)}
}

var contractItemRequired = []string{"contract_id", "title", "contract_type", "status", "effective_status", "starts_on", "expires_on", "signed_on", "revision"}

func contractDetailProperties() schema {
	p := contractListItemProperties()
	for _, k := range []string{"summary", "purpose", "consideration", "termination"} {
		p[k] = stringSchema(8192)
	}
	p["created_at"] = dateTimeSchema()
	p["updated_at"] = dateTimeSchema()
	p["parties"] = arraySchema(objectSchema(schema{"party_id": uuidSchema(), "role": stringSchema(32), "display_name": stringSchema(200), "kind": stringSchema(32), "own": booleanSchema(), "retired_at": schema{"anyOf": []schema{dateTimeSchema(), {"type": "null"}}}}, "party_id", "role", "display_name", "kind", "own", "retired_at"), 0)
	p["documents"] = arraySchema(objectSchema(schema{"node_id": integerSchema(1, contractSafeInteger), "content_version_id": uuidSchema(), "blob_sha256": sha256Schema(), "size": integerSchema(0, contractSafeInteger), "role": stringSchema(32), "added_at": dateTimeSchema(), "current": booleanSchema(), "available": booleanSchema()}, "node_id", "content_version_id", "blob_sha256", "size", "role", "added_at", "current", "available"), 0)
	return p
}
func listContractsSchemas() (schema, schema) {
	in := rootObjectSchema(schema{"status": enumSchema("draft", "pending_signature", "active", "expired", "terminated", "cancelled"), "contract_type": schema{"type": "string", "pattern": "^[a-z0-9_]{1,64}$"}, "party_id": uuidSchema(), "expiring_before": contractDateSchema(false), "q": stringSchema(0), "as_of": contractDateSchema(false), "limit": integerSchema(1, 100), "offset": integerSchema(0, contractSafeInteger)})
	out := rootObjectSchema(withPrivateCache(schema{"items": arraySchema(objectSchema(contractListItemProperties(), contractItemRequired...), 100), "limit": integerSchema(1, 100), "offset": integerSchema(0, contractSafeInteger), "as_of": contractDateSchema(false)}), cacheRequired("items", "limit", "offset", "as_of")...)
	return in, out
}
func getContractSchemas() (schema, schema) {
	required := append(append([]string{}, contractItemRequired...), "summary", "purpose", "consideration", "termination", "created_at", "updated_at", "parties", "documents")
	return rootObjectSchema(schema{"contract_id": uuidSchema(), "as_of": contractDateSchema(false)}, "contract_id"), rootObjectSchema(withPrivateCache(contractDetailProperties()), cacheRequired(required...)...)
}
func contractWriteReceiptSchema() schema {
	return rootObjectSchema(withPrivateCache(schema{"contract_id": uuidSchema(), "revision": integerSchema(1, contractSafeInteger)}), cacheRequired("contract_id", "revision")...)
}
func createContractSchemas() (schema, schema) {
	p := contractEditableProperties()
	parties := arraySchema(objectSchema(schema{"party_id": uuidSchema(), "role": enumSchema("own", "counterparty")}, "party_id", "role"), 0)
	parties["uniqueItems"] = true
	p["parties"] = parties
	return rootObjectSchema(p, "title"), contractWriteReceiptSchema()
}
func updateContractSchemas() (schema, schema) {
	patch := objectSchema(maps.Clone(contractEditableProperties()))
	patch["minProperties"] = 1
	return rootObjectSchema(schema{"contract_id": uuidSchema(), "revision": integerSchema(1, contractSafeInteger), "patch": patch}, "contract_id", "revision", "patch"), contractWriteReceiptSchema()
}
func linkContractDocumentSchemas() (schema, schema) {
	return rootObjectSchema(schema{"contract_id": uuidSchema(), "revision": integerSchema(1, contractSafeInteger), "node_id": integerSchema(1, contractSafeInteger), "content_version_id": uuidSchema(), "role": enumSchema("draft", "signed", "audit_trail", "amendment", "attachment")}, "contract_id", "revision", "node_id", "content_version_id", "role"), contractWriteReceiptSchema()
}
