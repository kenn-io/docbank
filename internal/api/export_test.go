package api

// SetAfterRouteMutation installs a hook after a move or restore commits and
// before its handler builds the response. Install it before serving requests.
func SetAfterRouteMutation(g *OperationGate, hook func()) {
	g.afterRouteMutation = hook
}

func SetAfterUploadVerification(g *OperationGate, hook func()) {
	g.afterUploadVerification = hook
}
