package document

const (
	PassageTagAvailabilityExact       = "exact"
	PassageTagAvailabilityUnavailable = "unavailable"
)

// WouldCreateConceptCycle reports whether adding parent -> child to a
// broader-to-narrower graph would create a cycle. Callers bound the graph and
// load it from the current concept authority before using this check.
func WouldCreateConceptCycle(edges map[string][]string, parent, child string) bool {
	queue := []string{child}
	seen := make(map[string]bool, len(edges))
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if node == parent {
			return true
		}
		if seen[node] {
			continue
		}
		seen[node] = true
		queue = append(queue, edges[node]...)
	}
	return false
}
