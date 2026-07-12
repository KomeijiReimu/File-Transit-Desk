package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionUploadEntrypointsRemainStreamingAndLegacyWrappersStayDeleted(t *testing.T) {
	legacyNames := []string{"save" + "Uploads", "saveFile" + "UniqueAtomic", "form" + "Files"}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob Go sources: %v", err)
	}
	for _, path := range files {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, name := range legacyNames {
			if strings.Contains(string(source), name) {
				t.Fatalf("deleted legacy upload wrapper %q remains in %s", name, path)
			}
		}
	}

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	functions := make(map[string]*ast.FuncDecl)
	for _, declaration := range parsed.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok {
			functions[fn.Name.Name] = fn
		}
	}
	for _, required := range []string{"saveStreamingMultipart", "saveRawUpload", "commitUploadCandidate"} {
		if functions[required] == nil {
			t.Fatalf("required streaming/upload safety function %q was removed", required)
		}
	}
	for _, name := range []string{"saveStreamingMultipart", "saveRawUpload", "uploadFiles", "uploadByLease", "publicUpload", "uploadRawByLease", "publicUploadRawByLease"} {
		fn := functions[name]
		if fn == nil {
			t.Fatalf("missing production upload entrypoint %q", name)
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if ok && (selector.Sel.Name == "Body" || selector.Sel.Name == "MultipartForm") {
				t.Errorf("production upload entrypoint %s uses whole-body API %s", name, selector.Sel.Name)
			}
			return true
		})
	}
	streaming := functions["saveStreamingMultipart"]
	foundReader := false
	ast.Inspect(streaming.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "NewReader" {
			if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == "multipart" {
				foundReader = true
			}
		}
		return true
	})
	if !foundReader {
		t.Fatalf("streaming multipart path no longer uses multipart.NewReader")
	}
}
