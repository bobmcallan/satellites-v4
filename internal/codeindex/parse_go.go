package codeindex

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

// parseGoFile extracts top-level declarations from a single Go source
// file. Returns Symbol entries keyed to repoRelPath (the path the
// caller will store on Symbol.File). The fset is shared across the
// caller's index pass so position information is consistent.
//
// Errors during parse are silently treated as zero-symbol files —
// half-broken sources should not abort an index pass; the caller logs
// errors via the indexer's error counter.
func parseGoFile(fset *token.FileSet, absPath, repoRelPath string) ([]Symbol, error) {
	file, err := parser.ParseFile(fset, absPath, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", repoRelPath, err)
	}
	out := make([]Symbol, 0)
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			out = append(out, funcSymbol(fset, d, repoRelPath))
		case *ast.GenDecl:
			out = append(out, genDeclSymbols(fset, d, repoRelPath)...)
		}
	}
	return out, nil
}

func funcSymbol(fset *token.FileSet, fd *ast.FuncDecl, file string) Symbol {
	start := fset.Position(fd.Pos())
	end := fset.Position(fd.End())
	name := fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		// Method — qualify with receiver type for searchability.
		name = receiverPrefix(fd.Recv.List[0]) + "." + name
	}
	return Symbol{
		ID:        symbolID(file, name, start.Line),
		Name:      name,
		Kind:      "func",
		Language:  "go",
		File:      file,
		StartLine: start.Line,
		EndLine:   end.Line,
		Signature: funcSignature(fd),
	}
}

func receiverPrefix(field *ast.Field) string {
	switch t := field.Type.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return "*" + id.Name
		}
	}
	return ""
}

func funcSignature(fd *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString("func ")
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		b.WriteString("(")
		b.WriteString(receiverPrefix(fd.Recv.List[0]))
		b.WriteString(") ")
	}
	b.WriteString(fd.Name.Name)
	b.WriteString("(...)")
	return b.String()
}

func genDeclSymbols(fset *token.FileSet, gd *ast.GenDecl, file string) []Symbol {
	out := make([]Symbol, 0)
	for _, spec := range gd.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			start := fset.Position(s.Pos())
			end := fset.Position(s.End())
			out = append(out, Symbol{
				ID:        symbolID(file, s.Name.Name, start.Line),
				Name:      s.Name.Name,
				Kind:      "type",
				Language:  "go",
				File:      file,
				StartLine: start.Line,
				EndLine:   end.Line,
			})
		case *ast.ValueSpec:
			kind := "var"
			if gd.Tok == token.CONST {
				kind = "const"
			}
			for _, name := range s.Names {
				if name.Name == "_" {
					continue
				}
				pos := fset.Position(name.Pos())
				end := fset.Position(s.End())
				out = append(out, Symbol{
					ID:        symbolID(file, name.Name, pos.Line),
					Name:      name.Name,
					Kind:      kind,
					Language:  "go",
					File:      file,
					StartLine: pos.Line,
					EndLine:   end.Line,
				})
			}
		}
	}
	return out
}

// symbolID hashes (file, name, startLine) for a stable id across
// reindex passes provided the source position is unchanged.
func symbolID(file, name string, line int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s:%s:%d", file, name, line)))
	return "sym_" + hex.EncodeToString(h[:6])
}
