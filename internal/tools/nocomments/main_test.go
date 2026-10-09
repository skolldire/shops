package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFindComments(t *testing.T) {
	tests := map[string]struct {
		src  string
		want int
	}{
		"line comment":            {"package x\n\n// note\nvar a = 1\n", 1},
		"todo without space":      {"package x\n\n//TODO fix\n", 1},
		"trailing after code":     {"package x\n\nvar a = 1// trailing\n", 1},
		"block comment":           {"package x\n\n/* block */\nvar a = 1\n", 1},
		"inline block":            {"package x\n\nvar a = /* x */ 1\n", 1},
		"two comments":            {"package x\n\n// one\nvar a = 1 // two\n", 2},
		"slashes inside string":   {"package x\n\nvar u = \"http://example.com // not a comment\"\n", 0},
		"block inside raw string": {"package x\n\nvar p = `/static/* and /* text */`\n", 0},
		"rune slash":              {"package x\n\nvar r = '/'\n", 0},
		"build directive":         {"//go:build integration\n\npackage x\n", 0},
		"embed directive":         {"package x\n\nimport \"embed\"\n\n//go:embed static\nvar fs embed.FS\n", 0},
		"generate is not allowed": {"package x\n\n//go:generate echo hi\n", 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.go")
			require.NoError(t, os.WriteFile(path, []byte(tt.src), 0o600))

			found, err := findComments(path)

			require.NoError(t, err)
			require.Len(t, found, tt.want, found)
		})
	}
}

func TestScanSkipsVendorAndGit(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{".git", "vendor", "pkg"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(root, dir, "x.go"), []byte("package x\n// c\n"), 0o600))
	}

	found, err := scan(root)

	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Contains(t, found[0], filepath.Join("pkg", "x.go"))
}
