package brainservice

func NewDriver(runner Runner) Driver { return newServiceDriver("windows", runner, taskState) }
