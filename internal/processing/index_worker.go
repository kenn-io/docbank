package processing

import "go.kenn.io/docbank/internal/vectorworker"

var ErrVectorIndexCandidateInvalid = vectorworker.ErrVectorIndexCandidateInvalid

type UnavailableVectorSet = vectorworker.UnavailableVectorSet
type UnavailableVectorCoverage = vectorworker.UnavailableVectorCoverage
type IndexRestoreReport = vectorworker.IndexRestoreReport
type IndexWorkerConfig = vectorworker.IndexWorkerConfig
type IndexWorker = vectorworker.IndexWorker
type IndexLease = vectorworker.IndexLease

var NewIndexWorker = vectorworker.NewIndexWorker
