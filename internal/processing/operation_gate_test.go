package processing

// TestOperationGate is the real daemon gate surface exercised by processing
// tests. Its factory is wired by the external test package to avoid an import
// cycle between same-package processing tests and backupapp.
type TestOperationGate = processingOperationGate

var testOperationGateFactory func() TestOperationGate

// SetTestOperationGateFactory installs the real gate constructor before tests
// execute. No processing test creates a gate during package initialization.
func SetTestOperationGateFactory(factory func() TestOperationGate) {
	testOperationGateFactory = factory
}

func newTestOperationGate() TestOperationGate {
	if testOperationGateFactory == nil {
		panic("processing test operation gate factory is not installed")
	}
	return testOperationGateFactory()
}
