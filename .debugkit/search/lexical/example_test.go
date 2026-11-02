package lexical_test

import (
	"fmt"

	"go.kenn.io/kit/search/lexical"
)

func ExampleCJK_IndexFor() {
	cjk := lexical.EnableCharacterPhrase()
	fmt.Println(cjk.Enabled())
	fmt.Println(cjk.IndexFor("running"))
	fmt.Println(cjk.IndexFor("run 搜索"))

	analyzer, ok := cjk.Analyzer("run 搜索")
	if !ok {
		panic("mixed query should use the CJK index")
	}
	prepared, err := analyzer.PrepareLiteral("run 搜索")
	if err != nil {
		panic(err)
	}
	fmt.Println(prepared.Match)
	// Output:
	// true
	// ordinary
	// cjk
	// "run" "搜 索"
}
