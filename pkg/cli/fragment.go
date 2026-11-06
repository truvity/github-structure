package cli

// fragment.go — the two commands of the company boundary: the provider
// exports an org's ceiling (`boundary`), and a company checks its fragment
// against it (`validate-fragment`) before the provider ever sees it. Both
// are thin over registry.Config.Boundary and registry.ValidateFragment, the
// functions registry.Compose runs, so the refusal is the same words in both
// places.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/truvity/github-structure/pkg/registry"
)

const (
	boundaryUsage = "usage: boundary (prints the --org's fragment ceiling and reserved names as JSON)"

	validateFragmentUsage = "usage: validate-fragment --owner <label> [--boundary <file>] <fragment.yaml>" +
		" (the boundary is a file `boundary` printed; without --boundary the configured registry is used)"
)

// boundary prints the ceiling and the reserved names of --org as JSON. It
// names things and carries no value: no credentials path, no owner, no
// variable value.
func (r *runner) boundary(org string, args []string) error {
	if len(args) != 0 {
		return usageError(boundaryUsage)
	}

	reg, _, err := r.lookup(org)
	if err != nil {
		return err
	}

	b, err := reg.Boundary(org)
	if err != nil {
		return err
	}

	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the boundary of %s: %w", org, err)
	}

	_, _ = fmt.Fprintf(r.stdout(), "%s\n", raw)

	return nil
}

// validateFragment holds a company's fragment file to a boundary and says
// nothing is wrong, or names every violation with its key path. The
// boundary is a file `boundary` printed (a company has nothing else), or,
// without one, the fragment's org in the registry this tool is configured
// with (the provider's own check).
func (r *runner) validateFragment(args []string) error {
	owner, boundaryPath, path, err := validateFragmentArgs(args)
	if err != nil {
		return err
	}

	b, err := readBoundary(boundaryPath)
	if err != nil {
		return err
	}

	frag, err := readFragment(owner, path)
	if err != nil {
		return err
	}

	if b == nil {
		reg, err := r.load()
		if err != nil {
			return errors.New("no boundary to validate against: pass --boundary <file> (a file `boundary` printed): " + err.Error())
		}

		got, err := reg.Boundary(frag.Org)
		if err != nil {
			return err
		}

		b = &got
	}

	if err := registry.ValidateFragment(*b, frag); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(r.stdout(), "org %q: fragment owner %q: ok\n", b.Org, owner)

	return nil
}

// readBoundary reads the --boundary file; nil means use the registry.
func readBoundary(boundaryPath string) (*registry.Boundary, error) {
	if boundaryPath == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(boundaryPath)
	if err != nil {
		return nil, fmt.Errorf("read the boundary: %w", err)
	}

	var b registry.Boundary

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("boundary %s: not a boundary as `boundary` prints it: %w", boundaryPath, err)
	}

	return &b, nil
}

func readFragment(owner, path string) (registry.Fragment, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return registry.Fragment{}, fmt.Errorf("read the fragment: %w", err)
	}

	frag, err := registry.ParseFragment(owner, raw)
	if err != nil {
		return registry.Fragment{}, fmt.Errorf("%s: %w", path, err)
	}

	return frag, nil
}

// validateFragmentArgs parses `validate-fragment --owner L [--boundary F] FILE`
// with the flags before or after the file.
func validateFragmentArgs(args []string) (owner, boundaryPath, path string, err error) {
	fset := flag.NewFlagSet("validate-fragment", flag.ContinueOnError)
	fset.SetOutput(io.Discard)
	fset.StringVar(&owner, "owner", "", "label of whoever wrote the fragment")
	fset.StringVar(&boundaryPath, "boundary", "", "a boundary file `boundary` printed")

	var pos []string

	for rest := args; ; {
		if perr := fset.Parse(rest); perr != nil {
			return "", "", "", usageError(validateFragmentUsage + ": " + perr.Error())
		}

		rest = fset.Args()
		if len(rest) == 0 {
			break
		}

		pos = append(pos, rest[0])
		rest = rest[1:]
	}

	if len(pos) != 1 || owner == "" {
		return "", "", "", usageError(validateFragmentUsage)
	}

	return owner, boundaryPath, pos[0], nil
}
