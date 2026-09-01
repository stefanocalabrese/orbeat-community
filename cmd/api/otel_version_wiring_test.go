package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestOTelServiceVersionIsNotALiteral closes audit finding C8.
//
// telemetry.Config's ServiceVersion was the string literal "dev" in BOTH
// binaries, while release.yml injects the real tag into internal/version via
// -ldflags. Every trace and metric emitted by a signed release image was
// therefore attributed to "dev", so no telemetry could be tied to the release
// that produced it. internal/version's own doc comment says it "must be the
// ONLY place the version is read from" and names three consumers; this was a
// silent fourth, and it was frozen.
//
// The gate is an AST check rather than a behavioural one because the value is
// only observable in an exported span or metric attribute, which needs a
// collector to read back. What CAN be asserted cheaply is the shape that made
// it inert: a literal cannot be injected by the linker, so a literal here is
// the defect by construction, whatever string it holds. Writing "0.3.0"
// instead of "dev" would be just as broken and this still fails.
//
// It covers both binaries from one place, because the defect was in both and
// fixing one is exactly how the sibling gets missed.
func TestOTelServiceVersionIsNotALiteral(t *testing.T) {
	for _, file := range []string{"main.go", "../gateway/main.go"} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		found := false
		ast.Inspect(f, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "ServiceVersion" {
				return true
			}
			found = true
			if lit, isLit := kv.Value.(*ast.BasicLit); isLit {
				t.Errorf(`%s sets telemetry ServiceVersion to the literal %s.

A literal cannot be injected by the linker, so every trace and metric from a signed
release image carries it forever regardless of the tag that built the image. Read it
from internal/version.Version, which release.yml's -ldflags actually targets (audit C8).`,
					file, lit.Value)
			}
			return true
		})

		if !found {
			t.Errorf(`%s sets no ServiceVersion at all, so this gate proved nothing about it.

Either the field was renamed (update this gate) or telemetry setup was removed from
this binary. It is not acceptable for this check to pass by finding nothing: that is
the shape the finding it closes had in the first place.`, file)
		}
	}
}
