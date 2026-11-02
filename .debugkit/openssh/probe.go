package openssh

import (
	"context"
	"errors"
)

type masterProbeState uint8

const (
	probeAbsent masterProbeState = iota
	probeAlive
	probeStale
)

func (m *PersistentManager) probeControlMasterWithRunner(
	ctx context.Context,
	socketPath string,
	target Target,
	runSSH RunSSH,
) (masterProbeState, error) {
	dialState, err := inspectControlSocket(ctx, socketPath)
	if err != nil {
		return probeAbsent, err
	}
	switch dialState {
	case socketAbsent:
		return probeAbsent, nil
	case socketStale:
		return probeStale, nil
	default:
	}

	arguments, err := CheckArguments(socketPath, target)
	if err != nil {
		return probeAbsent, err
	}
	exitCode, runErr := runSSH(ctx, arguments)
	if runErr == nil && exitCode == 0 {
		return probeAlive, nil
	}
	commandErr := &CommandError{
		Operation:   "master check",
		Destination: target.String(),
		ExitCode:    exitCode,
		Err:         runErr,
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		commandErr.Err = errors.Join(ctxErr, runErr)
		return probeAbsent, errors.Join(ErrProbeIndeterminate, commandErr)
	}
	if exitCode < 0 {
		return probeAbsent, errors.Join(ErrProbeIndeterminate, commandErr)
	}
	return probeAbsent, errors.Join(ErrControlPathOccupied, commandErr)
}
