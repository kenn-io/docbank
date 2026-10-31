package grounding

import "strings"

func canonical(integer, fraction string) string {
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if fraction != "" {
		integer += "." + fraction
	}
	return integer
}
func numberValues(s string) []string {
	if s == "" {
		return nil
	}
	for _, r := range s {
		if !digit(r) && !separator(r) {
			return nil
		}
	}
	if !strings.ContainsAny(s, ",.\u00a0\u202f'") {
		return []string{canonical(s, "")}
	}
	// Non-decimal grouping separators must delimit groups of three.
	for _, sep := range []string{"\u00a0", "\u202f", "'"} {
		if strings.Contains(s, sep) {
			decimalSuffix := ""
			if pos := strings.IndexAny(s, ",."); pos >= 0 {
				decimalSuffix = s[pos:]
				s = s[:pos]
			}
			parts := strings.Split(s, sep)
			for i, p := range parts {
				if len(p) < 1 || len(p) > 3 || i > 0 && len(p) != 3 {
					return nil
				}
				for _, r := range p {
					if !digit(r) {
						return nil
					}
				}
			}
			s = strings.Join(parts, "") + decimalSuffix
		}
	}
	// Group separators are permitted only in the validated integer groups.
	for _, r := range s {
		if !digit(r) && r != ',' && r != '.' {
			return nil
		}
	}
	commas, dots := strings.Count(s, ","), strings.Count(s, ".")
	if commas > 0 && dots > 0 {
		decimal := "."
		if strings.LastIndex(s, ",") > strings.LastIndex(s, ".") {
			decimal = ","
		}
		group := ","
		if decimal == "," {
			group = "."
		}
		parts := strings.Split(s, decimal)
		if len(parts) != 2 {
			return nil
		}
		if parts[1] == "" {
			return nil
		}
		for _, r := range parts[1] {
			if !digit(r) {
				return nil
			}
		}
		g := strings.Split(parts[0], group)
		if len(g[0]) < 1 || len(g[0]) > 3 {
			return nil
		}
		for _, p := range g[1:] {
			if len(p) != 3 {
				return nil
			}
		}
		return []string{canonical(strings.Join(g, ""), parts[1])}
	}
	sep := ","
	if dots > 0 {
		sep = "."
	}
	parts := strings.Split(s, sep)
	if len(parts) == 1 {
		return []string{canonical(s, "")}
	}
	grouped := len(parts[0]) >= 1 && len(parts[0]) <= 3
	for _, p := range parts[1:] {
		if len(p) != 3 {
			grouped = false
		}
	}
	if len(parts) > 2 {
		if !grouped {
			return nil
		}
		return []string{canonical(strings.Join(parts, ""), "")}
	}
	if parts[1] == "" {
		return nil
	}
	if grouped && canonical(strings.Join(parts, ""), "") != canonical(parts[0], parts[1]) {
		// Preserve the separator when grouping and decimal readings differ.
		return []string{"literal:" + s}
	}
	return []string{canonical(parts[0], parts[1])}
}
