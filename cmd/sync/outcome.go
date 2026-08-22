package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/stefanocalabrese/orbeat-community/internal/syncclient"
)

// syncOutcome is the machine-readable result of one `sync` run. It exists so
// reconcileAll can COMPUTE a result that renderers then present, rather than
// printing as it goes: with printing inlined there was no value to serialise,
// and runSync's orchestration — the seam where v1.14.1's abort-cascade defect
// hid — could only be tested by capturing stdout, a test that breaks on wording
// changes and passes on logic changes.
//
// A section is nil when its reconciler NEVER RAN because an earlier one aborted
// fatally. That is deliberately distinct from a section that ran and found
// nothing (zero counts, empty slices): on the exit-2 path "we never got to
// rules" is the single most useful fact, and a flattened representation cannot
// express it.
type syncOutcome struct {
	ExitCode int     `json:"exitCode"`
	Fatal    *string `json:"fatal"`
	// DryRun is true when this run was `sync --dry-run`: every section's
	// counters and Changes describe what WOULD happen, and nothing was written.
	DryRun          bool              `json:"dryRun"`
	Artifacts       *artifactsSection `json:"artifacts"`
	Seeds           *blocksSection    `json:"seeds"`
	Rules           *blocksSection    `json:"rules"`
	RestartRequired bool              `json:"restartRequired"`
}

// artifactsSection mirrors syncclient.ReconcileResult. Its vocabulary
// (added/updated/removed) differs from blocksSection's (written/stripped)
// because it manages whole FILES, while seeds and rules manage managed BLOCKS
// inside files the user owns. The names are deliberately NOT normalised: the
// operations genuinely differ, and a shared vocabulary would flatten that and
// add a translation layer that can drift from the structs it mirrors.
type artifactsSection struct {
	Handled   int      `json:"handled"`
	Added     int      `json:"added"`
	Updated   int      `json:"updated"`
	Unchanged int      `json:"unchanged"`
	Removed   int      `json:"removed"`
	Skipped   []string `json:"skipped"`
	Warnings  []string `json:"warnings"`
	Failures  []string `json:"failures"`
	// Changes is the dry-run plan for this section: every intended mutation,
	// in recording order, with the manifest write filtered out (see
	// filterManifest). Always [] rather than null when the section ran — see
	// strs — including on a REAL (non-dry) run, so a consumer never needs a
	// dry-run-vs-not branch just to range over it.
	Changes []syncclient.Change `json:"changes"`
}

// blocksSection mirrors syncclient.SeedResult and syncclient.RulesResult, which
// have identical shapes.
type blocksSection struct {
	Written   int      `json:"written"`
	Unchanged int      `json:"unchanged"`
	Stripped  int      `json:"stripped"`
	Warnings  []string `json:"warnings"`
	Failures  []string `json:"failures"`
	// Changes — see artifactsSection.Changes.
	Changes []syncclient.Change `json:"changes"`
}

// strs normalises a nil slice to an empty one so it serialises as [] rather
// than null. A consumer must be able to range over warnings/failures without a
// nil check, and a null here would OVERLOAD the one meaning this schema
// reserves for null: "the section never ran" (syncOutcome's doc above). A
// warnings/failures null and a section null sit at different JSON paths, so a
// schema-aware consumer could tell them apart — but a single reserved meaning
// per null is simpler to consume correctly than a meaning that depends on
// which field you're looking at.
func strs(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// planManifestName mirrors internal/syncclient's unexported manifestName
// (".orbeat-sync-manifest.json"). The manifest write is bookkeeping — it
// exists in every Plan (syncclient.saveManifest routes it through the same
// guarded writeAtomic as any other write, deliberately, so Plan.Changes stays
// the complete and honest record) — but it is not a user-facing change, so the
// reporting layer here is what filters it out. Matched on the base name since
// the constant itself isn't exported.
const planManifestName = ".orbeat-sync-manifest.json"

// sectionChanges turns a *syncclient.Plan (nil-safe: a real, non-planning run
// passes nil) into the slice a section reports: the manifest bookkeeping
// entry dropped, and normalised to [] rather than null so it serialises the
// same way on both a dry run and a real one — see strs for why null is
// reserved for a different meaning in this schema.
func sectionChanges(p *syncclient.Plan) []syncclient.Change {
	out := make([]syncclient.Change, 0, len(p.Changes()))
	for _, c := range p.Changes() {
		if filepath.Base(c.Path) == planManifestName {
			continue
		}
		out = append(out, c)
	}
	return out
}

// renderHuman writes the summary exactly as reconcileAll printed it before the
// outcome struct existed. The format strings are copied verbatim; any change
// here is a user-visible change to a stable CLI surface. Verbatim covers the
// BYTES, not the TIMING: reconcileAll used to print each section as its
// reconciler finished, so a run interrupted mid-seeds left the artifacts
// section already on the terminal; now nothing prints until reconcileAll
// returns and runSync calls this function once, so an interrupted run leaves
// nothing printed at all.
func renderHuman(w io.Writer, o *syncOutcome) {
	if o.DryRun {
		fmt.Fprintln(w, "DRY RUN — no files were written.")
	}
	if a := o.Artifacts; a != nil {
		fmt.Fprintf(w, "Synced %d artifact(s): %d added, %d updated, %d unchanged, %d removed.\n",
			a.Handled, a.Added, a.Updated, a.Unchanged, a.Removed)
		for _, s := range a.Skipped {
			fmt.Fprintf(w, "  skipped (a non-orbeat file already exists): %s\n", s)
		}
		renderNotes(w, a.Warnings, a.Failures)
		renderChanges(w, a.Changes)
	}
	if s := o.Seeds; s != nil {
		fmt.Fprintf(w, "Seeds: %d written, %d unchanged, %d stripped.\n", s.Written, s.Unchanged, s.Stripped)
		renderNotes(w, s.Warnings, s.Failures)
		renderChanges(w, s.Changes)
	}
	if r := o.Rules; r != nil {
		fmt.Fprintf(w, "Rules: %d written, %d unchanged, %d stripped.\n", r.Written, r.Unchanged, r.Stripped)
		renderNotes(w, r.Warnings, r.Failures)
		renderChanges(w, r.Changes)
	}
	if o.RestartRequired {
		fmt.Fprintln(w, "Agent set changed — restart Claude Code (or start a new prompt) to pick up the change.")
	}
}

func renderNotes(w io.Writer, warnings, failures []string) {
	for _, x := range warnings {
		fmt.Fprintf(w, "  warning: %s\n", x)
	}
	for _, x := range failures {
		fmt.Fprintf(w, "  failed: %s\n", x)
	}
}

// renderChanges prints one line per intended mutation, operation then path.
// A Change is only ever a path and an operation (syncclient.Plan's doc) — no
// invented phrasing like "block removed" or "import added" belongs here.
func renderChanges(w io.Writer, changes []syncclient.Change) {
	for _, c := range changes {
		fmt.Fprintf(w, "  %s %s\n", c.Op, c.Path)
	}
}

// renderJSON writes the outcome as exactly one JSON object followed by a single
// newline. Indented for a human who pipes it to a file; jq does not care either
// way.
func renderJSON(w io.Writer, o *syncOutcome) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(o)
}
