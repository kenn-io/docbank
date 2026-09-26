package agentops

import "testing"

func TestAgentPolicyReadDoesNotAuthorizeDurableWork(t *testing.T) {
	for _, class := range []Class{Read, Session} {
		if !PolicyRead.Allows(class) {
			t.Fatalf("read refused %s", class)
		}
	}
	for _, class := range []Class{Write, Admin, Class("unknown")} {
		if PolicyRead.Allows(class) {
			t.Fatalf("read allowed %s", class)
		}
	}
	if Policy("unknown").Allows(Read) {
		t.Fatal("unknown policy allowed")
	}
	if PolicyWrite.Allows(Admin) {
		t.Fatal("write allowed admin")
	}
}
