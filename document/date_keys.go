package document

import (
	"regexp"
	"time"
)

// DateKey is the rebuildable search projection of one event.
type DateKey struct {
	CivilDate, CivilEndDate, CivilMonth, CivilYear string
	Floating, Sensitive                            bool
}

var calendarDateSource = regexp.MustCompile(`^(.*?/)?(calendar\.event\.e[0-9]{6})\.(start|end)(?:\.raw)?(?:/[0-9]+)?$`)

func calendarSibling(source string) (string, string) {
	m := calendarDateSource.FindStringSubmatch(source)
	if m == nil {
		return "", ""
	}
	return m[1] + m[2], m[3]
}

// DateKeysFor derives keys together so an all-day start can use its sibling end.
func DateKeysFor(events []DocumentEventV1) ([]DateKey, error) {
	keys := make([]DateKey, len(events))
	ends := make(map[string]string)
	for _, event := range events {
		component, side := calendarSibling(event.SourceKey)
		if event.DateKind == "ended" && event.Precision == "date" && side == "end" {
			ends[component] = event.DateValue
		}
	}
	for i, event := range events {
		civil, err := eventCivil(event.DateValue, event.Precision)
		if err != nil {
			return nil, err
		}
		key := DateKey{CivilYear: event.DateValue[:4], Sensitive: event.Sensitive}
		for _, actor := range event.Actors {
			key.Sensitive = key.Sensitive || actor.Sensitive
		}
		if event.Precision != "year" {
			key.CivilMonth = event.DateValue[:7]
		}
		if event.Precision != "year" && event.Precision != "month" {
			key.CivilDate = event.DateValue[:10]
			key.CivilEndDate = key.CivilDate
		}
		key.Floating = event.Precision != "year" && event.Precision != "month" && event.Precision != "date" && event.UTCKey == ""
		component, side := calendarSibling(event.SourceKey)
		if event.DateKind == "started" && event.Precision == "date" && side == "start" {
			if end, ok := ends[component]; ok && end > key.CivilDate {
				parsed, err := time.Parse("2006-01-02", end)
				if err != nil {
					return nil, err
				}
				key.CivilEndDate = parsed.AddDate(0, 0, -1).Format("2006-01-02")
			}
		}
		_ = civil
		keys[i] = key
	}
	return keys, nil
}
