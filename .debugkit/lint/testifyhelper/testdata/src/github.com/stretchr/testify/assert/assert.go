package assert

import (
	"errors"
	"testing"
)

var AnError = errors.New("assertion example")

type Assertions struct{}

func New(*testing.T) *Assertions { return &Assertions{} }

func Equal(*testing.T, any, any, ...any) bool { return true }
func True(*testing.T, bool, ...any) bool      { return true }

func (*Assertions) Equal(any, any, ...any) bool { return true }
func (*Assertions) True(bool, ...any) bool      { return true }

func ObjectsAreEqual(any, any) bool { return true }

func Mismatched(*testing.T, string) bool { return true }
func (*Assertions) Mismatched(int) bool  { return true }
