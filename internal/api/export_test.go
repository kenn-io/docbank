package api

// SetAfterRouteMutation installs a hook that runs after each successful route
// mutation and before its handler builds the response. Install it before the
// server starts handling requests.
func SetAfterRouteMutation(g *OperationGate, hook func()) {
	g.afterRouteMutation = hook
}
