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
