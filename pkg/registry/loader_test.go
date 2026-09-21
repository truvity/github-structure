package registry

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadRejectsUnknownFields(t *testing.T) {
	fsys := fstest.MapFS{
		"test.yaml": &fstest.MapFile{
			Data: []byte("known: value\nunknown_field: bad\n"),
		},
	}

	var dst struct {
		Known string `yaml:"known"`
	}

	assert.Error(t, load(fsys, "test.yaml", &dst))
}

func TestLoadAcceptsKnownFields(t *testing.T) {
	fsys := fstest.MapFS{
		"test.yaml": &fstest.MapFile{
			Data: []byte("known: value\n"),
		},
	}

	var dst struct {
		Known string `yaml:"known"`
	}

	require.NoError(t, load(fsys, "test.yaml", &dst))
	assert.Equal(t, "value", dst.Known)
}

func TestLoadReturnsErrorOnMissingFile(t *testing.T) {
	fsys := fstest.MapFS{}

	var dst struct{}

	assert.Error(t, load(fsys, "missing.yaml", &dst))
}

func TestLoadReturnsErrorOnMalformedYAML(t *testing.T) {
	fsys := fstest.MapFS{
		"bad.yaml": &fstest.MapFile{
			Data: []byte(":\n  - :\n  bad: [unmatched"),
		},
	}

	var dst struct {
		Bad string `yaml:"bad"`
	}

	assert.Error(t, load(fsys, "bad.yaml", &dst))
}

// The base has to be complete, because it is the only thing standing
// between a preset's silence and an unmanaged field. A field added to
// RepoSettings and forgotten here would resolve to the zero value —
// false, "" — and the engine would apply THAT, which is worse than
// leaving it alone.
func TestGitHubDefaultsAreComplete(t *testing.T) {
	missing := gitHubDefaults().missingFields()
	assert.Empty(t, missing,
		"gitHubDefaults must set every field a preset may omit; add the new field to base.go with GitHub's own answer")
}
