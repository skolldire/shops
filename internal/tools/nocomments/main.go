package main

import (
	"errors"
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var allowedDirectives = []string{"//go:build ", "//go:embed "}

var skippedDirs = map[string]bool{".git": true, "vendor": true, "node_modules": true}

func main() {
	found, err := scan(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if len(found) > 0 {
		fmt.Fprintln(os.Stderr, strings.Join(found, "\n"))
		fmt.Fprintln(os.Stderr, "Go comments are not allowed")
		os.Exit(1)
	}
}

func scan(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && skippedDirs[d.Name()]:
			return filepath.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go"):
			return nil
		}
		comments, err := findComments(path)
		found = append(found, comments...)
		return err
	})
	return found, err
}

func findComments(path string) ([]string, error) {
	src, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	file := fset.AddFile(path, -1, len(src))
	var (
		s        scanner.Scanner
		found    []string
		scanErrs []error
	)
	s.Init(file, src, func(pos token.Position, msg string) {
		scanErrs = append(scanErrs, fmt.Errorf("%s: %s", pos, msg))
	}, scanner.ScanComments)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.COMMENT && !isDirective(lit) {
			found = append(found, fmt.Sprintf("%s: %s", fset.Position(pos), strings.SplitN(lit, "\n", 2)[0]))
		}
	}
	return found, errors.Join(scanErrs...)
}

func isDirective(comment string) bool {
	for _, prefix := range allowedDirectives {
		if strings.HasPrefix(comment, prefix) {
			return true
		}
	}
	return false
}
