package brainservice

func NewDriver(runner Runner) Driver { return newServiceDriver("linux", runner, nil) }
