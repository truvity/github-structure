package staterename

import (
	"errors"

	"github.com/truvity/github-structure/pkg/registry"
)

// Move is one resource whose URN changed.
type Move struct {
	From, To string
}

// Report says what a Rekey did.
type Report struct {
	From, To   string
	Noop       bool
	Moved      []Move
	References int
	Resources  int
}

// Rekey is not implemented yet.
func Rekey(_ []byte, _ string, _ *registry.Config, _, _ string) ([]byte, Report, error) {
	return nil, Report{}, errors.New("not implemented")
}
