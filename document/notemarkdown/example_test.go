package notemarkdown_test

import (
	"context"
	"fmt"

	"go.kenn.io/docbank/document/notemarkdown"
)

func ExampleParse() {
	// This standalone example has no caller context.
	ctx := context.Background()
	src := []byte("# Start\n[[A:notes.md#Intro|next]]\n")
	note, err := notemarkdown.Parse(ctx, src, notemarkdown.Limits{})
	if err != nil {
		panic(err)
	}
	h := note.Headings[0]
	fmt.Printf("%s (%s): %s", note.Title, note.TitleSource, src[h.Start:h.LineEnd])
	fmt.Printf("%s -> %s #%s\n", note.Links[0].Label, note.Links[0].Target, note.Links[0].Fragment)
	// Output:
	// Start (h1): # Start
	// next -> A:notes.md #Intro
}
