//go:build !linux

package sandbox

import "context"

const NativeRunnerIdentity = "sha256:afc3202c30a20fbb62fe6b7a4ccf282dea0ba16e40d22e7bdfa3f1d28819db16"

type Runner interface {
	Identity() string
	Run(ctx context.Context, request Request) (Result, error)
}

func NewNativeRunner() (Runner, error) { return nil, ErrUnavailable }

func Run(context.Context, Request) (Result, error) { return Result{}, ErrUnavailable }
