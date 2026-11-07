package brainservice

func NewDriver(runner Runner) Driver { return newServiceDriver("darwin", runner, nil) }
