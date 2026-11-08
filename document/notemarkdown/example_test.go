package notemarkdown_test

import (
	"context"
	"crypto/sha256"
	"fmt"

	"go.kenn.io/docbank/document/notemarkdown"
)

func Example() {
	ctx := context.Background()
	note, err := notemarkdown.Parse(ctx, []byte("# Start\n[[Target#Intro]]\n"))
	if err != nil {
		panic(err)
	}
	canonical, err := notemarkdown.CanonicalArtifact(ctx, note)
	if err != nil {
		panic(err)
	}
	checksum := sha256.Sum256(canonical)
	query, err := notemarkdown.PlanTarget("area/start.md", note.Links[0])
	if err != nil {
		panic(err)
	}
	// An integration supplies this evidence from one authorized snapshot, using
	// real immutable node/version bindings instead of these synthetic values.
	evidence := notemarkdown.CandidateSet{NamespaceGeneration: "revision-7", Complete: true, Candidates: []notemarkdown.Candidate{{Binding: "target-version-1", Path: "area/Target.md", Kind: "note", Keys: []notemarkdown.LookupKey{query.Keys[0]}}}}
	result, err := notemarkdown.Resolve(ctx, query, evidence)
	if err != nil {
		panic(err)
	}
	// Load only result.Binding's exact ready parse generation in an integration.
	target, err := notemarkdown.Parse(ctx, []byte("# Intro\n"))
	if err != nil {
		panic(err)
	}
	fragment, err := notemarkdown.ResolveFragment(ctx, query.Fragment, target.Headings)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%x %s %s\n", checksum[:4], result.State, fragment.State)
	// Output: b25470d1 resolved resolved
}
