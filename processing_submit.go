package docbank

import (
	"context"
	"errors"
	"slices"

	internalprocessing "go.kenn.io/docbank/internal/processing"
)

type submitProcessingResult struct {
	job internalprocessing.Job
	err error
}

// SubmitProcessing returns once the requested processing job reaches durable
// enqueue acknowledgement. Accepted work continues under the service lease.
func (v *Vault) SubmitProcessing(ctx context.Context, request StartProcessingRequest) (ProcessingJob, error) {
	if err := v.begin(); err != nil {
		return ProcessingJob{}, err
	}
	started := make(chan internalprocessing.Job, 1)
	finished := make(chan submitProcessingResult, 1)
	go func() {
		defer v.lifecycle.RUnlock()
		job, err := v.processing.StartWithProgress(ctx, internalprocessing.StartRequest{
			Selector:        toProcessingSelector(request.PlanRequest.Selector),
			PlanFingerprint: request.PlanFingerprint,
			Consent:         request.Consent,
		}, func(job internalprocessing.Job) {
			select {
			case started <- job:
			default:
			}
		})
		finished <- submitProcessingResult{job: job, err: err}
	}()

	for {
		select {
		case job := <-started:
			return fromProcessingJob(job), nil
		case result := <-finished:
			select {
			case job := <-started:
				return fromProcessingJob(job), nil
			default:
			}
			if result.err != nil {
				return ProcessingJob{}, result.err
			}
			return fromProcessingJob(result.job), nil
		case <-ctx.Done():
			select {
			case job := <-started:
				return fromProcessingJob(job), nil
			case result := <-finished:
				if result.err == nil {
					return fromProcessingJob(result.job), nil
				}
				return ProcessingJob{}, errors.Join(result.err, ctx.Err())
			default:
			}
			return ProcessingJob{}, ctx.Err()
		}
	}
}

func fromProcessingJob(job internalprocessing.Job) ProcessingJob {
	return ProcessingJob{ID: job.ID, RenditionJobID: job.RenditionJobID, AttachmentID: job.AttachmentID,
		EmbeddingJobIDs: slices.Clone(job.EmbeddingJobIDs), ProfileFingerprint: job.ProfileFingerprint,
		ContentVersionID: job.ContentVersionID}
}
