package main

import "testing"

func TestContractSignCommands(t *testing.T) {
	cmd := newContractSignCommand()
	for _, args := range [][]string{{"sign", "check"}, {"sign", "templates"}} {
		found, _, err := cmd.Find(args)
		if err != nil || found.Name() != args[0] {
			t.Fatalf("find %v: %v", args, err)
		}
	}
}
