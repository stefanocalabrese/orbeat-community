package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestReportedCascadeClaimsResolveToCode is A10 one level up: it checks that
// the CLAIMS in inboundForeignKeys (cascade_index_test.go) are true, where
// TestInboundForeignKeysOnParentTables beside it only checks that the SET of
// foreign keys is complete.
//
// The gap that left open is the same defect in prose. A migration could add a
// cascading child of role, add the map entry the FK gate demands, write
// "reported as RevokedGrants.Whatever" in its reason, and go green while a
// role deletion went on destroying those rows with nobody told. Every claim
// this repo has lost has been lost that way: a rationale is read as context
// by a reviewer who verifies the function beside it.
//
// WHAT "REPORTED" MEANS IS RESOLVED ALL THE WAY TO THE OPERATOR, not to the
// nearest struct. A field on RevokedGrants proves only that a number exists in
// a Go value; nothing outside this package ever reads that value except
// handleDeleteRole, and the finding A10 recorded is about the role.delete
// audit record, which is the surface whose stated protection is "legibility,
// not prevention". So a reportedAs(...) entry has to survive four separate
// resolutions, and each closes a different way of being false:
//
//  1. Every named field EXISTS on store.RevokedGrants. Catches a renamed or
//     deleted field whose claim was left behind.
//  2. Every named field is ASSIGNED by DeleteRole's returned literal. Catches
//     a field that exists but is never populated, which reports a zero value
//     as if it were a measurement.
//  3. DeleteRole READS the child table. This is the one the whole gate is for:
//     it is what a fabricated "reported" entry fails, because writing the
//     claim is free and adding the query is not.
//  4. Every named field appears in handleDeleteRole's role.delete audit
//     metadata (internal/api/admin_roles.go). Catches the value being computed
//     correctly and never leaving the process, which is what "reported" would
//     mean to nobody.
//
// NONE OF IT RESOLVES AGAINST THIS FILE OR AGAINST cascade_index_test.go, and
// that is checked rather than intended: sourcesUnderTest refuses to parse a
// _test.go file at all. Grepping "VirtualKeys" against the file that declares
// the map would pass on every possible input, which is this repo's oldest
// failure shape (a test that cannot fail).
//
// THE VACUITY GUARDS ARE THE REST OF IT. Four derivations feed this, and each
// one can come back empty for a reason that has nothing to do with the
// property: a moved function, a renamed struct, a refactor that hoists the
// metadata map into a helper. An empty derived set compared against a claim
// finds no violations and reads exactly like a pass. So each derivation fails
// loudly when it comes back empty, and each failure says the scan is broken
// rather than that the code is fine. Measured: replacing the audit metadata
// with an empty map literal, which compiles, makes this test FAIL on the guard
// instead of passing over nine unreported children.
//
// WHAT IT STILL CANNOT CATCH, stated because the next person will assume
// otherwise: it resolves NAMES, not values. `UsageRows: keyCount` satisfies
// every one of the four checks above while reporting the virtual-key count as
// a usage-row count. The runtime gates for that are
// TestDeleteRoleReportsTheCascadesNobodyWasCounting (this package) and
// TestAdminDeleteRoleAuditsEveryCascadingChild (internal/api), and neither is
// derived from this map, so a SIXTH child added tomorrow gets these four
// structural checks automatically and gets a value assertion only if somebody
// writes one.
func TestReportedCascadeClaimsResolveToCode(t *testing.T) {
	storeFiles := sourcesUnderTest(t, ".")
	apiFiles := sourcesUnderTest(t, filepath.Join("..", "api"))

	revokedFields := structFieldNames(t, storeFiles, "RevokedGrants")
	deleteRole := soleFuncNamed(t, storeFiles, "DeleteRole")
	assigned := returnedStructKeys(t, deleteRole, "RevokedGrants")
	readTables := tablesReadBy(t, deleteRole)

	handler := soleFuncNamed(t, apiFiles, "handleDeleteRole")
	audited := auditedRevokedFields(t, handler)

	// A map with no reported entry at all would satisfy every loop below
	// without executing one of them, so the claim that gives this test its
	// name has to exist before anything is checked against it.
	claims := 0
	for _, byName := range inboundForeignKeys {
		for _, fk := range byName {
			if fk.reported {
				claims++
			}
		}
	}
	if claims == 0 {
		t.Fatal("inboundForeignKeys declares no reportedAs(...) entry at all. Every " +
			"assertion in this test iterates that set, so an empty one makes the whole " +
			"gate silent while store.RevokedGrants goes on claiming to describe a role " +
			"deletion. If reporting really was dropped, delete this test with a reason " +
			"rather than letting it pass over nothing")
	}

	for parent, byName := range inboundForeignKeys {
		for name, fk := range byName {
			t.Run(parent+"/"+name, func(t *testing.T) {
				if strings.TrimSpace(fk.why) == "" {
					t.Errorf("constraint %q on %s carries no reason at all. Even the "+
						"unchecked half of an entry has to say which migration added it",
						name, parent)
				}

				if !fk.reported {
					if len(fk.fields) != 0 {
						t.Errorf("constraint %q on %s is unreported yet names fields %v: "+
							"one of the two is a lie and the gate cannot tell which",
							name, parent, fk.fields)
					}
					if parent == "role" && fk.onDelete == "cascade" {
						t.Errorf("constraint %q makes %s(%s) a CASCADING child of role and "+
							"declares it unreported. That is the exact state A10 found: "+
							"migrations 00020 and 00022 each added one and store.RevokedGrants "+
							"went on describing two of five children for two releases, so a "+
							"role deletion killed robot credentials, quotas and metering with "+
							"the audit record silent. Report it (add the read to DeleteRole, "+
							"the field to RevokedGrants, the key to handleDeleteRole's "+
							"metadata) and declare it with reportedAs. If it genuinely must "+
							"not be reported, that is a change to THIS gate with a written "+
							"reason, not a value you pick in the map",
							name, fk.child, fk.columns)
					}
					return
				}

				if parent != "role" {
					t.Fatalf("constraint %q on %s is declared reportedAs, but the fields it "+
						"names live on store.RevokedGrants, which describes a ROLE deletion "+
						"and nothing else. %s has no equivalent report, so there is nothing "+
						"for this claim to resolve against", name, parent, parent)
				}
				if len(fk.fields) == 0 {
					t.Fatalf("constraint %q on role is declared reportedAs with no field "+
						"names, so the claim has nothing to resolve and every check below "+
						"would pass vacuously", name)
				}

				if !readTables[fk.child] {
					t.Errorf("constraint %q claims the rows it destroys in %s are reported, "+
						"but DeleteRole never reads %s. The tables it does read are %v, "+
						"derived from the FROM/JOIN/INTO/UPDATE targets in its own SQL. A "+
						"count nobody computes is reported as zero, which is worse than "+
						"reporting nothing: it is an audit record that positively states no "+
						"rows were destroyed",
						name, fk.child, fk.child, sortedKeys(readTables))
				}

				for _, f := range fk.fields {
					if !revokedFields[f] {
						t.Errorf("constraint %q claims RevokedGrants.%s, which is not a field "+
							"of that struct. Its fields are %v (internal/store/rbac.go)",
							name, f, sortedKeys(revokedFields))
						continue
					}
					if !assigned[f] {
						t.Errorf("constraint %q claims RevokedGrants.%s and the field exists, "+
							"but DeleteRole's returned literal never sets it, so every caller "+
							"reads the zero value. Assigned there: %v",
							name, f, sortedKeys(assigned))
					}
					if !audited[f] {
						t.Errorf("constraint %q claims RevokedGrants.%s is REPORTED, but "+
							"handleDeleteRole's role.delete audit metadata does not carry it "+
							"(internal/api/admin_roles.go). A value that never leaves the "+
							"process is reported to nobody, and the audit record is the "+
							"surface A10 is about: it is where an operator answers \"why did "+
							"alice lose access?\". Carried today: %v",
							name, f, sortedKeys(audited))
					}
				}
			})
		}
	}
}

// sourcesUnderTest parses every non-test .go file directly inside dir.
//
// _test.go files are excluded, and that is the point rather than a
// convenience: inboundForeignKeys and this gate both live in _test.go files in
// this package, so parsing them would let a claim resolve against its own
// spelling. Nested directories are not walked, because every function this
// gate reads is at the top level of its package.
func sourcesUnderTest(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatalf("no non-test .go files under %s: the derivation has nothing to read, "+
			"so every claim resolved against it would pass without being checked", dir)
	}
	return files
}

// structFieldNames returns the field names of the named struct type, searched
// across the whole package rather than in one file, so moving RevokedGrants
// out of rbac.go relocates the gate with it instead of blinding it.
func structFieldNames(t *testing.T, files []*ast.File, name string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	seen := false
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != name {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			seen = true
			for _, field := range st.Fields.List {
				for _, id := range field.Names {
					found[id.Name] = true
				}
			}
			return false
		})
	}
	if !seen {
		t.Fatalf("no struct type %s in this package. It was renamed, moved out of the "+
			"package, or turned into an alias; whichever it was, this gate is now reading "+
			"an empty field set and would accept any claim at all", name)
	}
	if len(found) == 0 {
		t.Fatalf("struct %s has no named fields, so every field claim resolved against it "+
			"is compared with an empty set", name)
	}
	return found
}

// soleFuncNamed finds the one function or method called name. More than one
// is a hard failure rather than a first-match, because picking silently would
// make the gate read a different function from the one its message names.
func soleFuncNamed(t *testing.T, files []*ast.File, name string) *ast.FuncDecl {
	t.Helper()
	var hits []*ast.FuncDecl
	for _, f := range files {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Body != nil {
				hits = append(hits, fn)
			}
		}
	}
	switch len(hits) {
	case 1:
		return hits[0]
	case 0:
		t.Fatalf("no function %s with a body in the parsed sources: it was renamed or "+
			"moved, and everything this gate derives from it is now empty", name)
	default:
		t.Fatalf("%d functions named %s in the parsed sources; this gate cannot tell which "+
			"one it is supposed to read", len(hits), name)
	}
	return nil
}

// returnedStructKeys collects the field names set by `return <structName>{...}`
// composite literals inside fn.
func returnedStructKeys(t *testing.T, fn *ast.FuncDecl, structName string) map[string]bool {
	t.Helper()
	keys := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		for _, res := range ret.Results {
			lit, ok := res.(*ast.CompositeLit)
			if !ok {
				continue
			}
			if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != structName {
				continue
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if id, ok := kv.Key.(*ast.Ident); ok {
					keys[id.Name] = true
				}
			}
		}
		return true
	})
	if len(keys) == 0 {
		t.Fatalf("%s returns no keyed %s{...} literal, so the set of fields it populates "+
			"is empty and every claim about one would be checked against nothing",
			fn.Name.Name, structName)
	}
	return keys
}

// sqlReadTarget matches the table name after FROM, JOIN, INTO or UPDATE.
//
// Tokenised on those keywords rather than by searching the SQL for the table
// name anywhere in it, for two reasons that both bite here: `entitlement` is a
// substring of `artifact_entitlement`, so a substring search reports a read
// that never happened; and a bare identifier search would accept a table named
// only in a column alias or a comment.
var sqlReadTarget = regexp.MustCompile(`(?i)\b(?:from|join|into|update)\s+([a-zA-Z_][a-zA-Z0-9_]*)`)

// tablesReadBy returns the tables named as a FROM/JOIN/INTO/UPDATE target in
// any string literal inside fn. Comments are invisible to this by
// construction, since only *ast.BasicLit strings are scanned, which is what
// keeps a doc comment mentioning a table from satisfying a claim about it.
func tablesReadBy(t *testing.T, fn *ast.FuncDecl) map[string]bool {
	t.Helper()
	tables := map[string]bool{}
	literals := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		literals++
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		for _, m := range sqlReadTarget.FindAllStringSubmatch(s, -1) {
			tables[strings.ToLower(m[1])] = true
		}
		return true
	})
	if literals == 0 {
		t.Fatalf("%s contains no string literals at all: its SQL moved somewhere this "+
			"scan cannot see, so the set of tables it reads is empty and every "+
			"\"DeleteRole reads this child\" check would pass", fn.Name.Name)
	}
	if len(tables) == 0 {
		t.Fatalf("%s has %d string literals and none names a FROM/JOIN/INTO/UPDATE "+
			"target, so this gate believes it reads no table at all",
			fn.Name.Name, literals)
	}
	return tables
}

// auditedRevokedFields returns the RevokedGrants fields that reach the
// role.delete audit metadata, derived from handleDeleteRole's own source on
// both sides: the variable name comes from the assignment whose right-hand
// side calls DeleteRole, and the field names come from the selectors on that
// variable inside the literal keyed Metadata. Renaming the variable moves the
// scan with it; hard-coding "g" would have left the gate reading nothing the
// first time somebody renamed it.
func auditedRevokedFields(t *testing.T, fn *ast.FuncDecl) map[string]bool {
	t.Helper()

	revoked := ""
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) == 0 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "DeleteRole" {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
			revoked = id.Name
		}
		return false
	})
	if revoked == "" {
		t.Fatalf("%s never assigns the result of a DeleteRole(...) call to a named "+
			"variable, so this gate cannot tell which selectors carry the revoked "+
			"grants and would report an empty audit set", fn.Name.Name)
	}

	var metadata ast.Expr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Metadata" {
			metadata = kv.Value
			return false
		}
		return true
	})
	if metadata == nil {
		t.Fatalf("%s builds no literal keyed Metadata: the audit record's metadata moved "+
			"out of this function (a helper, a method on the struct), so this gate reads "+
			"an empty set and every \"is it reported\" check passes. Point it at wherever "+
			"the metadata is built now", fn.Name.Name)
	}

	fields := map[string]bool{}
	ast.Inspect(metadata, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == revoked {
			fields[sel.Sel.Name] = true
		}
		return true
	})
	if len(fields) == 0 {
		t.Fatalf("%s's Metadata literal reads no field of %s at all. Either the audit "+
			"record stopped describing the cascade entirely, or it now reaches those "+
			"values by some route this scan cannot follow; both make this gate silent",
			fn.Name.Name, revoked)
	}
	return fields
}

// sortedKeys renders a derived set for a failure message. Sorted because an
// unordered map printed in a diff is noise, and the point of these messages is
// that a reader can compare what was claimed against what was found.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
