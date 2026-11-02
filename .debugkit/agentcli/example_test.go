package agentcli_test

import (
	"fmt"

	"go.kenn.io/kit/agentcli"
)

func ExampleAdapter_Resume() {
	agent, err := agentcli.NewCodex(agentcli.Command{
		Executable: "codex",
		Options:    []string{"--profile", "team"},
	})
	if err != nil {
		panic(err)
	}

	invocation, err := agent.Resume("session-1", agentcli.Request{})
	if err != nil {
		panic(err)
	}
	fmt.Println(invocation.Argv)
	// Output: [codex --profile team resume session-1]
}

func ExampleAdapter_Start() {
	agent, err := agentcli.New(agentcli.OpenCode, agentcli.Command{})
	if err != nil {
		panic(err)
	}
	prompt := "review this change"
	invocation, err := agent.Start(agentcli.Request{
		Mode:         agentcli.NonInteractive,
		Prompt:       agentcli.Prompt{Text: prompt},
		OutputFormat: agentcli.OutputJSONL,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(invocation.Argv)
	fmt.Println(*invocation.Stdin)
	// Output:
	// [opencode run --format json]
	// review this change
}
