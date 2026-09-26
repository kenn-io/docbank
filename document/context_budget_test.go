package document

import (
	"reflect"
	"testing"
)

func TestSelectWithinBudget(t *testing.T) {
	got := SelectWithinBudget([]int{7, 5, 2}, 9)
	if !reflect.DeepEqual(got, []int{0, 2}) {
		t.Fatal(got)
	}
	if len(SelectWithinBudget([]int{1}, 0)) != 0 {
		t.Fatal("budget exceeded")
	}
}
