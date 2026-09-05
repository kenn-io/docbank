package mistral

import "io"

type localUnitCounter func(io.ReaderAt, int64) (int, error)

// localUnitCounters is the Mistral-owned authority registry. Only formats
// with provider-authentic unit evidence may be added here.
var localUnitCounters = map[string]localUnitCounter{}

func countLocalUnits(format CandidateFormat, reader io.ReaderAt, size int64) (int, error) {
	counter := localUnitCounters[format.ID]
	if counter == nil {
		return 0, nil
	}
	return counter(reader, size)
}
