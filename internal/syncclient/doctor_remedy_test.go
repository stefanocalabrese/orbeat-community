package syncclient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A9: two doctor remedies ended with "it will rebuild it and re-fetch your
// entitled artifacts", and that was measured false: the rebuilt ledger came
// back empty and every rendered artifact froze. Adoption made the common case
// true; these gates pin the parts of the claim that are STILL not true, so the
// sentence cannot quietly go back to promising a full restore.
//
// Both remedies are also asserted to come from one place. They stated the same
// false thing in two files, which is how one fix could have left the other
// standing.

// remedyClaims are the qualifications rebuildManifestRemedy must keep. Each is
// a real limit re-derived from the reconcilers, not a phrasing preference: a
// differing file is still skipped, and the rules ledger is not reconstructible.
var remedyClaims = []string{"skipped", "rules ledger"}

func TestRebuildManifestRemedyStatesWhatARebuildDoesNotRestore(t *testing.T) {
	got := rebuildManifestRemedy("remove the manifest and run 'orbeat-sync sync'")
	for _, claim := range remedyClaims {
		if !strings.Contains(got, claim) {
			t.Fatalf("remedy drops the %q qualification, so it over-promises again:\n%s", claim, got)
		}
	}
	if strings.Contains(got, "re-fetch your entitled artifacts") && !strings.Contains(got, "not a restore") {
		t.Fatalf("remedy is back to the unqualified promise A9 falsified:\n%s", got)
	}
}

// TestBothManifestRemediesAreQualified drives the two real findings rather than
// the helper, so a caller that stops using the helper and writes its own
// sentence still fails.
func TestBothManifestRemediesAreQualified(t *testing.T) {
	t.Run("unparseable manifest", func(t *testing.T) {
		home := t.TempDir()
		must(t, os.MkdirAll(home, 0o755))
		must(t, os.WriteFile(filepath.Join(home, manifestName), []byte("{not json"), 0o644))
		assertQualifiedRemedy(t, Diagnose(home, nil, "", "", ""), CheckManifest, "cannot be read or parsed")
	})

	t.Run("Files entry escaping the sync root", func(t *testing.T) {
		home := t.TempDir()
		must(t, saveManifest(home, manifest{Files: []string{"../escape.md"}}, nil))
		assertQualifiedRemedy(t, Diagnose(home, nil, "", "", ""), CheckManifest, "escapes the sync root")
	})
}

func assertQualifiedRemedy(t *testing.T, rep Report, check Check, detailSubstr string) {
	t.Helper()
	for _, f := range rep.Findings {
		if f.Check != check || !strings.Contains(f.Detail, detailSubstr) {
			continue
		}
		for _, claim := range remedyClaims {
			if !strings.Contains(f.Remedy, claim) {
				t.Fatalf("remedy for %q drops the %q qualification:\n%s", detailSubstr, claim, f.Remedy)
			}
		}
		return
	}
	t.Fatalf("no %q finding whose detail mentions %q; findings = %+v", check, detailSubstr, rep.Findings)
}

// TestDoctorNotesAFilesLedgerEntrySyncWillNeverWrite covers the doctor half of
// A7. The drift check's own remedy ("run 'orbeat-sync sync' to recreate it")
// became false for a shape-invalid entry the moment Reconcile started refusing
// them, so such an entry must get its own note and must NOT be reported as
// drift.
func TestDoctorNotesAFilesLedgerEntrySyncWillNeverWrite(t *testing.T) {
	home := t.TempDir()
	must(t, saveManifest(home, manifest{Files: []string{"CLAUDE.md"}}, nil))

	rep := Diagnose(home, nil, "", "", "")

	var note *Finding
	for i := range rep.Findings {
		f := &rep.Findings[i]
		if f.Check == CheckLedgerDrift {
			t.Fatalf("a shape-invalid entry must not be reported as drift, whose remedy tells the operator sync will recreate it: %+v", *f)
		}
		if f.Check == CheckManifest && f.Path == "CLAUDE.md" {
			note = f
		}
	}
	if note == nil {
		t.Fatalf("want a CheckManifest note for the tampered entry, got %+v", rep.Findings)
	}
	if note.Severity != SeverityNote {
		t.Errorf("severity = %q, want a note: sync refuses the entry and drops it, so nothing is at risk", note.Severity)
	}
}
