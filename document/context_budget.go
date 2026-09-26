package document

// SelectWithinBudget keeps positive-cost candidates in their original order,
// skipping those that do not fit the remaining byte budget.
func SelectWithinBudget(costs []int, budget int) []int {
	selected := make([]int, 0)
	for index, cost := range costs {
		if cost > 0 && cost <= budget {
			selected = append(selected, index)
			budget -= cost
		}
	}
	return selected
}
