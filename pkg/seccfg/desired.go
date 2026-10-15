package seccfg

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

// DesiredFile is the standalone desired-state file: only the
// repositories it names are managed, everything else is left alone.
//
//	repos:
//	  example-repo: Some Configuration
type DesiredFile struct {
	Repos map[string]string `yaml:"repos"`
}

// ParseDesired reads a DesiredFile strictly: an unknown key is an error.
func ParseDesired(data []byte) (map[string]string, error) {
	var f DesiredFile

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse desired file: %w", err)
	}

	for repo, cfg := range f.Repos {
		if repo == "" || cfg == "" {
			return nil, fmt.Errorf("parse desired file: repo %q: configuration name must not be empty", repo)
		}
	}

	return f.Repos, nil
}
