package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Scan local construction boundaries, including MCP's direct error envelopes.
// Dynamic types in the constructor and API passthrough remain valid; literal
// local categories must use a registered constant.
func TestLocalErrorTypesCoverCreationSites(t *testing.T) {
	registered := map[string]bool{}
	for _, typ := range localErrorTypes {
		if registered[typ] {
			t.Errorf("duplicate local error type %q", typ)
		}
		registered[typ] = true
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	constants := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed[name] = file
		for _, decl := range file.Decls {
			group, ok := decl.(*ast.GenDecl)
			if !ok || group.Tok != token.CONST {
				continue
			}
			for _, spec := range group.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if !strings.HasPrefix(name.Name, "errorType") {
						continue
					}
					literal, ok := value.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						t.Fatalf("%s must be a string constant", name.Name)
					}
					typ, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					constants[name.Name] = typ
					if !registered[typ] {
						t.Errorf("%s (%q) missing from localErrorTypes", name.Name, typ)
					}
				}
			}
		}
	}
	used := map[string]bool{}
	check := func(expr ast.Expr) {
		switch value := expr.(type) {
		case *ast.Ident:
			if typ, exists := constants[value.Name]; exists && registered[typ] {
				used[typ] = true
				return
			}
			// The common constructor forwards its caller's category.
			if value.Name == "typ" && filepath.Base(fset.Position(expr.Pos()).Filename) == "types.go" {
				return
			}
		case *ast.SelectorExpr:
			// Wrapping an existing error retains its original category.
			if value.Sel.Name == "Type" {
				return
			}
		}
		t.Errorf("%s: local error category must use a registered errorType constant", fset.Position(expr.Pos()))
	}
	for _, file := range parsed {
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				if name, ok := node.Fun.(*ast.Ident); ok && name.Name == "cliError" && len(node.Args) >= 2 {
					check(node.Args[1])
				}
			case *ast.CompositeLit:
				if name, ok := node.Type.(*ast.Ident); ok && name.Name == "CLIError" {
					for _, element := range node.Elts {
						if field, ok := element.(*ast.KeyValueExpr); ok {
							if key, ok := field.Key.(*ast.Ident); ok && key.Name == "Type" {
								check(field.Value)
							}
						}
					}
				}
				for _, element := range node.Elts {
					field, ok := element.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := field.Key.(*ast.BasicLit)
					if !ok || key.Value != `"error"` {
						continue
					}
					if envelope, ok := field.Value.(*ast.CompositeLit); ok {
						for _, element := range envelope.Elts {
							if field, ok := element.(*ast.KeyValueExpr); ok {
								if key, ok := field.Key.(*ast.BasicLit); ok && key.Value == `"type"` {
									check(field.Value)
								}
							}
						}
					}
				}
			case *ast.AssignStmt:
				for i, left := range node.Lhs {
					if field, ok := left.(*ast.SelectorExpr); ok && field.Sel.Name == "Type" && i < len(node.Rhs) {
						// The API parser and redactor assign dynamic passthrough types.
						// New static categories still need a registered constant.
						switch value := node.Rhs[i].(type) {
						case *ast.BasicLit:
							check(value)
						case *ast.Ident:
							if strings.HasPrefix(value.Name, "errorType") {
								check(value)
							}
						}
					}
				}
			}
			return true
		})
	}
	for typ := range registered {
		if !used[typ] {
			t.Errorf("registered error type %q has no local construction site", typ)
		}
	}
}

func TestErrorCatalogTypesMergeSnapshotAndLocalTypes(t *testing.T) {
	doc, err := loadOpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	properties := doc.Components.Schemas["ErrorObject"]["properties"].(map[string]any)
	apiTypes := properties["type"].(map[string]any)["enum"].([]any)
	if len(apiTypes) == 0 {
		t.Fatal("snapshot has no public API error types")
	}
	want := slices.Clone(localErrorTypes)
	for _, typ := range apiTypes {
		want = append(want, typ.(string))
	}
	slices.Sort(want)
	want = slices.Compact(want)
	if got := errorCatalogTypes(); !slices.Equal(got, want) {
		t.Fatalf("catalog types = %v, want sorted union %v", got, want)
	}
}
