package document

import (
	"regexp"
	"strings"
	"time"
)

// DateKey is the rebuildable search projection of one event.
type DateKey struct {
	CivilDate, CivilEndDate, CivilMonth, CivilYear string
	Floating, Sensitive                            bool
}

var calendarDateSource = regexp.MustCompile(`^calendar\.event\.e[0-9]{6}\.(start|end)(?:\.raw)?$`)

// CalendarDateSide identifies the date side of a calendar metadata field.
func CalendarDateSide(key string) string {
	if m := calendarDateSource.FindStringSubmatch(key); m != nil {
		return m[1]
	}
	return ""
}

func calendarSibling(source string) (string, string) {
	key := source
	prefix := ""
	if strings.HasPrefix(key, "metadata/") {
		parts := strings.Split(key, "/")
		if len(parts) != 4 {
			return "", ""
		}
		prefix = strings.Join(parts[:2], "/") + "/"
		key = parts[2]
	}
	side := CalendarDateSide(key)
	if side == "" {
		return "", ""
	}
	key = strings.TrimSuffix(key, ".raw")
	return prefix + strings.TrimSuffix(key, "."+side), side
}

// DateKeysFor derives keys together so an all-day start can use its sibling end.
func DateKeysFor(events []DocumentEventV1) ([]DateKey, error) {
	keys := make([]DateKey, len(events))
	ends := make(map[string]string)
	starts := make(map[string]string)
	for _, event := range events {
		component, side := calendarSibling(event.SourceKey)
		if event.DateKind == "started" && event.Precision == "date" && side == "start" {
			starts[component] = event.DateValue
		}
		if event.DateKind == "ended" && event.Precision == "date" && side == "end" {
			ends[component] = event.DateValue
		}
	}
	for i, event := range events {
		if _, err := eventCivil(event.DateValue, event.Precision); err != nil {
			return nil, err
		}
		key := DateKey{CivilYear: event.DateValue[:4], Sensitive: eventHasSensitiveEvidence(event)}
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
		if event.DateKind == "ended" && event.Precision == "date" && side == "end" {
			if start, ok := starts[component]; ok && event.DateValue > start {
				parsed, err := time.Parse("2006-01-02", event.DateValue)
				if err != nil {
					return nil, err
				}
				key.CivilDate = parsed.AddDate(0, 0, -1).Format("2006-01-02")
				key.CivilEndDate = key.CivilDate
				key.CivilMonth = key.CivilDate[:7]
				key.CivilYear = key.CivilDate[:4]
			}
		}
		keys[i] = key
	}
	return keys, nil
}
