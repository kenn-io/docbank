package grounding

import "strings"

func canonical(integer, fraction string, neg bool) string {
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if fraction != "" {
		integer += "." + fraction
	}
	if neg && integer != "0" {
		integer = "-" + integer
	}
	return integer
}
func numberValues(s string) []string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return nil
	}
	for _, r := range s {
		if !digit(r) && !separator(r) {
			return nil
		}
	}
	if !strings.ContainsAny(s, ",.\u00a0\u202f'") {
		if len(s) >= 2 && s[0] == '0' {
			if neg {
				return []string{"-" + s}
			}
			return []string{s}
		}
		return []string{canonical(s, "", neg)}
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
		return []string{canonical(strings.Join(g, ""), parts[1], neg)}
	}
	sep := ","
	if dots > 0 {
		sep = "."
	}
	parts := strings.Split(s, sep)
	if len(parts) == 1 {
		return []string{canonical(s, "", neg)}
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
		return []string{canonical(strings.Join(parts, ""), "", neg)}
	}
	if parts[1] == "" {
		return nil
	}
	dec := canonical(parts[0], parts[1], neg)
	if grouped {
		v := canonical(strings.Join(parts, ""), "", neg)
		if v != dec {
			return []string{v, dec}
		}
	}
	return []string{dec}
}

// bareNumberValues is used only for the amount value-only diagnostic.
func bareNumberValues(text string) []string {
	positive, negative, plusMinus := false, false, false
	if strings.HasPrefix(text, "+") {
		positive = true
		text = text[1:]
	} else if strings.HasPrefix(text, "±") {
		plusMinus = true
		text = strings.TrimPrefix(text, "±")
	} else if strings.HasPrefix(text, "-") {
		negative = true
		text = text[1:]
	} else if strings.HasPrefix(text, "−") {
		negative = true
		text = strings.TrimPrefix(text, "−")
	}
	values := numberValues(text)
	for i, value := range values {
		values[i] = scaleDecimal(value, 0, negative)
		if positive {
			values[i] = "+" + values[i]
		}
		if plusMinus {
			values[i] = "±" + values[i]
		}
		if negative && values[i] == "0" {
			values[i] = "-0"
		}
	}
	return values
}
