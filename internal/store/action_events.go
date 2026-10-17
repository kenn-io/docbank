package store

func validActionSurface(surface string) bool {
	switch surface {
	case "", "web", "tui", "cli":
		return true
	default:
		return false
	}
}
