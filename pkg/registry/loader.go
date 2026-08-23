package registry

// loader.go — strict YAML loading (unknown keys rejected), folded in
// verbatim from gitops cfg/loader so the registry stays a single package.

import (
	"bytes"
	"fmt"
	"io/fs"

	"go.yaml.in/yaml/v3"
)

// Load reads a YAML file from the given filesystem and unmarshals it into dst.
// Unknown YAML keys are rejected to prevent silent misconfiguration.
func load(fsys fs.FS, filename string, dst any) error {
	data, err := fs.ReadFile(fsys, filename)
	if err != nil {
		return fmt.Errorf("cfg: read %s: %w", filename, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("cfg: parse %s: %w", filename, err)
	}

	return nil
}
