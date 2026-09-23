package production

import "context"

// Worker is explicitly invoked by lifecycle integration. There is no global
// supervisor registration in J.
type Worker struct {
	Store    JobStore
	Renderer Renderer
	WorkerID string
}

func (w Worker) Run(ctx context.Context, request JobRequest) (Job, error) {
	return Run(ctx, w.Store, request, w.WorkerID, w.Renderer)
}
