package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPeopleCLIWorkflow(t *testing.T) {
	for _, name := range []string{"list", "show", "custodians", "create", "rename", "retire", "merge", "split"} {
		command, _, err := peopleCmd.Find([]string{name})
		require.NoError(t, err)
		require.Equal(t, name, command.Name())
	}

	oldRevision, oldName, oldIdentityIDs, oldAssignmentIDs, oldExternal := peopleRevision, peopleDisplayName, peopleIdentityIDs, peopleAssignmentIDs, peopleExternal
	defer func() {
		peopleRevision, peopleDisplayName, peopleIdentityIDs, peopleAssignmentIDs, peopleExternal = oldRevision, oldName, oldIdentityIDs, oldAssignmentIDs, oldExternal
	}()
	peopleRevision = 1
	peopleDisplayName = "Synthetic split"
	peopleIdentityIDs = nil
	peopleAssignmentIDs = nil
	peopleExternal = nil
	err := peopleSplitCmd.RunE(peopleSplitCmd, []string{"00000000-0000-4000-8000-000000000001"})
	require.Equal(t, exitUsage, commandExitCode(err, false))
}
