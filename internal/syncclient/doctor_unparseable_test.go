package syncclient

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiagnoseReportsAnUnparseableProjectsFile pins the half the
// malformed-ENTRY check left open.
//
// checkProjectsFile used to `return` silently when loadProjectsWithInvalid
// failed, so the one state where the file is present and completely unreadable
// produced no finding at all. That is the worst case to be silent about: every
// other project-related check in the Report then runs against an EMPTY project
// list, and their silence reads as health.
//
// An ABSENT file is deliberately still silent, because it is the ordinary state
// of a machine with no registered projects, and loadProjectsWithInvalid returns
// a nil error for it.
func TestDiagnoseReportsAnUnparseableProjectsFile(t *testing.T) {
	dir := t.TempDir()
	pj := filepath.Join(dir, "projects.json")
	if err := os.WriteFile(pj, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rep := Diagnose(t.TempDir(), nil, "", "", pj)

	var found *Finding
	for i := range rep.Findings {
		if rep.Findings[i].Check == CheckProjectsFile {
			found = &rep.Findings[i]
			break
		}
	}
	if found == nil {
		t.Fatalf(`an unparseable projects.json produced NO %s finding.

Every project-related check in this Report just ran against an empty project list,
because the file could not be read. Reporting nothing makes that silence look like a
clean tree.

Findings: %+v`, CheckProjectsFile, rep.Findings)
	}
	if found.Severity != SeverityProblem {
		t.Errorf("severity = %v, want %v: nothing self-heals here, and the file will not "+
			"repair itself on the next sync", found.Severity, SeverityProblem)
	}
	if found.Remedy == "" {
		t.Error("finding carries no remedy, so it tells an operator what is wrong and not what to do")
	}
}

// TestDiagnoseStaysSilentOnAnAbsentProjectsFile is the non-vacuity half. A
// machine with no registered projects is the ordinary case, and turning it into
// a problem would make `doctor` cry wolf on every fresh install, which is how a
// diagnostic stops being read.
func TestDiagnoseStaysSilentOnAnAbsentProjectsFile(t *testing.T) {
	rep := Diagnose(t.TempDir(), nil, "", "", filepath.Join(t.TempDir(), "does-not-exist.json"))
	for _, f := range rep.Findings {
		if f.Check == CheckProjectsFile {
			t.Errorf("an ABSENT projects.json produced a %s finding: %+v", CheckProjectsFile, f)
		}
	}
}
