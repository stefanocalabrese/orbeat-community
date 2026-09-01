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
	DryRun    bool              `json:"dryRun"`
	Artifacts *artifactsSection `json:"artifacts"`
	Seeds     *blocksSection    `json:"seeds"`
	Rules     *blocksSection    `json:"rules"`
	// Report is the deployment report this run filed, or nil when the stage
	// never ran: a dry run, a fatal abort, or a server that does not record
	// deployments. The section's own doc comment carries the taxonomy.
	Report          *reportSection `json:"report"`
	RestartRequired bool           `json:"restartRequired"`
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
	// Pins names every locally held pin whose revision was NOT honoured
	// exactly, computed by pinOutcomes: {name, requested, served, reason}
	// per entry. A pin served exactly needs no line, mirroring the human
	// Warnings this feeds (see runSync): precedent-following, not new
	// machinery, the same way Changes sits beside []string Warnings above
	// for the same reason. Always [] rather than null, on every run
	// (pinned or not, dry or real), for the same reason Changes is.
	Pins []pinOutcome `json:"pins"`
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

// reportSection is the outcome of filing this run's applied set with the
// server: rows recorded, entries the server dropped because the caller is no
// longer entitled to them, and anything that went wrong on the way.
//
// IT CARRIES NO Failures FIELD, AND THAT ABSENCE IS THE TAXONOMY. A failed
// report is a Warning at exit 0, never a Failure: ReconcileResult.Failures
// means "units that should have synced but did not" and cmd/sync maps any
// failure to exit 1, retryable. A report that did not arrive is not that.
// Every file the run was asked to deliver was delivered, and only the
// bookkeeping about it is missing, so filing it as a Failure would tell a cron
// loop to re-fetch and re-reconcile every artifact because a POST returned
// 502. With no field to put one in, there is no shape in which a report
// failure can reach the exit code: the rule is enforced by the type rather
// than by a reviewer noticing.
//
// It is top-level rather than a member of artifactsSection, which is
// documented as mirroring ReconcileResult, because a report covers what all
// three reconcilers applied.
type reportSection struct {
	Recorded int      `json:"recorded"`
	Dropped  int      `json:"dropped"`
	Warnings []string `json:"warnings"`
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

// pinOutcome is one locally held pin whose revision was not honoured exactly.
// Name is "<type>/<name>", the same label pin list/pin remove take on the
// command line, not the bare artifact name, since a "reason" without the
// type is ambiguous the moment two artifact types share a name. Reason is
// one of the three server-computed values a pinned syncArtifactDTO carries
// ("floor", "pruned", "ahead", internal/api/sync_pins.go) or one of the two
// CLIENT-ONLY reasons below, for a pin that was never sent to the server at
// all.
type pinOutcome struct {
	Name      string `json:"name"`
	Requested int    `json:"requested"`
	Served    int    `json:"served"`
	Reason    string `json:"reason"`
}

// pinOutcomeUnsupported and pinOutcomeUnknown are the two reasons a pin's
// outcome is decided ENTIRELY on this client, without the server ever seeing
// ?pin= for it: the server explicitly reported pinning:false, or runSync
// could not even ask (the /v1/sync/config fetch itself failed). Both are
// forced uniformly onto every held pin ("warns per pin", Gate 9) rather
// than read per-artifact from Artifact.PinOverride, which the server never
// set in either case because it never received the pin.
const (
	pinOutcomeUnsupported = "unsupported"
	pinOutcomeUnknown     = "unknown"
)

// pinOutcomes reports, for every locally held pin whose artifact came back in
// this fetch, whether it was honoured. forcedReason, when non-empty,
// overrides every entry's reason uniformly (see the two consts above); when
// empty, each entry's reason is read from that artifact's own PinOverride,
// and a pin served exactly (PinOverride == "") is omitted entirely: this
// function reports OVERRIDDEN pins only, matching the human warnings it
// feeds (runSync).
//
// A pin naming an artifact absent from arts (a revoked entitlement, or a
// visibility flip) is silently skipped: nothing here can distinguish that
// from a stale local pin left over after re-entitlement, and the ordinary
// sync report (added/updated/removed) already tells the developer her set of
// files changed.
func pinOutcomes(pins []syncclient.Pin, arts []syncclient.Artifact, forcedReason string) []pinOutcome {
	byID := make(map[string]syncclient.Artifact, len(arts))
	for _, a := range arts {
		byID[a.ID] = a
	}
	out := make([]pinOutcome, 0, len(pins))
	for _, p := range pins {
		a, ok := byID[p.ArtifactID]
		if !ok {
			continue
		}
		reason := forcedReason
		if reason == "" {
			reason = a.PinOverride
		}
		if reason == "" {
			continue
		}
		out = append(out, pinOutcome{
			Name: p.Type + "/" + p.Name, Requested: p.Revision, Served: a.Revision, Reason: reason,
		})
	}
	return out
}

// reportedPinned computes, for the deployment report, which applied artifacts
// this install holds BECAUSE a local pin named them and the server served
// them at exactly the pinned revision (spec sec 9.4). It is the ONLY place
// that decision is made; syncclient.ReportDeployments just looks values up by
// artifact id (its own doc comment says so).
//
// supported gates the WHOLE computation, not merely individual entries, and
// this is the primary defence against the skew trap, not
// deploymentReportItem's `json:"pinned,omitempty"` tag: a server this run
// could not confirm supports pinning must never see a true value at all. A
// nil scfgErr is a precondition the caller already checked (reportDeployments
// returns before reaching this call whenever scfgErr != nil), so the one
// thing left to ask is scfg.Pinning itself. false or unset returns nil,
// which makes every lookup in ReportDeployments answer false and, by way of
// omitempty, put nothing about pinning on the wire at all: belt AND braces,
// stated separately because either one moving alone (a future refactor that
// forgets this gate, or a tag edit that drops omitempty) must not silently
// become the other's job.
//
// Only HONOURED pins are added. An overridden pin (a.PinOverride != "", spec
// sec 4.2, floor/pruned/ahead) is left absent rather than added as false:
// the zero value and an explicit false are the same fact to a map lookup, so
// there is nothing to gain from writing it, and the distinction pinned:false
// vs pinned:true carries is "was this pin honoured", not "does a pin exist".
func reportedPinned(pins []syncclient.Pin, served []syncclient.Artifact, supported bool) map[string]bool {
	if !supported || len(pins) == 0 {
		return nil
	}
	byID := make(map[string]syncclient.Artifact, len(served))
	for _, a := range served {
		byID[a.ID] = a
	}
	out := make(map[string]bool, len(pins))
	for _, p := range pins {
		a, ok := byID[p.ArtifactID]
		if !ok || a.PinOverride != "" {
			continue
		}
		out[a.ID] = true
	}
	return out
}

// pinOutcomeWarnings renders one human warning line per overridden pin, in
// the same order pinOutcomes produced them.
func pinOutcomeWarnings(pins []pinOutcome) []string {
	out := make([]string, 0, len(pins))
	for _, p := range pins {
		out = append(out, pinOutcomeWarning(p))
	}
	return out
}

// pinOutcomeWarning renders one overridden pin: the artifact, what was
// requested, what was served instead, and why. The two client-only reasons
// get a full sentence, because there is no server-side explanation to lean
// on; the three server-computed reasons (floor/pruned/ahead) print verbatim
// rather than through a translation table here: cmd/sync does not import
// internal/api, so hardcoding their wording would drift the moment the
// server's own explanation changes.
func pinOutcomeWarning(o pinOutcome) string {
	switch o.Reason {
	case pinOutcomeUnsupported:
		return fmt.Sprintf("%s: held at revision %d, but the server does not support pinning; this sync served revision %d instead", o.Name, o.Requested, o.Served)
	case pinOutcomeUnknown:
		return fmt.Sprintf("%s: held at revision %d, but could not determine whether the server supports pinning; this sync served revision %d instead", o.Name, o.Requested, o.Served)
	default:
		return fmt.Sprintf("%s: held at revision %d, this sync served revision %d instead (%s)", o.Name, o.Requested, o.Served, o.Reason)
	}
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
	if rep := o.Report; rep != nil {
		// Warnings only, and through renderNotes' warning arm alone: the nil
		// second argument is reportSection's missing Failures field restated
		// at the call site. A successful report prints nothing, because the
		// user asked for a sync and got one, and the counts are in --json for
		// anything that wants them.
		renderNotes(w, rep.Warnings, nil)
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
