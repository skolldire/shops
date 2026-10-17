package app_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/skolldire/shops/internal/ordering/internal/app"
)

func sourceFiles(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		files = append(files, f)
	}
	require.NotEmpty(t, files)
	return files
}

func checkoutOutcomeConsts(t *testing.T, files []*ast.File) map[string]string {
	t.Helper()
	consts := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Outcome") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					require.True(t, ok, "%s is a string literal", name.Name)
					value, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					consts[name.Name] = value
				}
			}
		}
	}
	return consts
}

func TestEveryCheckoutOutcomeTheUseCaseReturnsIsCanonical(t *testing.T) {
	files := sourceFiles(t)
	consts := checkoutOutcomeConsts(t, files)
	canonical := app.CheckoutOutcomes()
	require.Len(t, canonical, len(slices.Compact(slices.Sorted(slices.Values(canonical)))), "no outcome is listed twice")

	used := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				return false
			case *ast.FuncDecl:
				return n.Name.Name != "CheckoutOutcomes"
			case *ast.Ident:
				if _, isOutcome := consts[n.Name]; isOutcome {
					used[n.Name] = true
				}
			}
			return true
		})
	}
	require.NotEmpty(t, used)
	for name := range used {
		require.Contains(t, canonical, consts[name], "%s is returned by the checkout but has no series from the start", name)
	}
	for name, value := range consts {
		require.True(t, used[name], "%s is declared but never returned", name)
		require.Contains(t, canonical, value)
	}
	require.Len(t, consts, len(canonical), "every canonical outcome is an Outcome constant")
}
