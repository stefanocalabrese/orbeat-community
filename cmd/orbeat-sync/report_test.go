package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stefanocalabrese/orbeat-community/internal/syncclient"
)

// These gates cover the deployment report: what the client sends, when it
// declines to send anything, and the one rule that governs a report that does
// not arrive.
//
// A gate that only proves a POST HAPPENED cannot see a wrong payload, and a
// wrong payload is the whole failure mode worth catching here: a client that
// reported the artifacts it was SERVED rather than the ones it APPLIED would
// pass every "did we call the endpoint" assertion while telling an operator
// that a developer is on a revision whose bytes never touched her disk. Every
// gate below therefore asserts the decoded body, artifact by artifact, with
// revisions that are deliberately distinct and never 1 so a client writing a
// constant fails on the value rather than passing on the shape.

const (
	idFmt     = "11111111-1111-4111-8111-111111111111"
	idRev     = "22222222-2222-4222-8222-222222222222"
	idMine    = "33333333-3333-4333-8333-333333333333"
	idRule    = "44444444-4444-4444-8444-444444444444"
	idPin     = "55555555-5555-4555-8555-555555555555"
	idPinnedA = "66666666-6666-4666-8666-666666666666"
	idPinnedB = "77777777-7777-4777-8777-777777777777"
)

// pinnableArtifact is a single-artifact fixture for the pin-command gates
// (artifact-version-pinning plan, Task 6): it carries a distinct id from
// mixedArtifacts' four so a test built on it can never collide with theirs
// by accident, and it advertises a revision window (oldestServable/latest)
// so --revision validation has something real to check against.
const pinnableArtifact = `{"artifacts":[
	{"id":"` + idPin + `","revision":3,"type":"skill","name":"pinme","content":"V3","oldestServable":1,"latest":3}
]}`

// pinnedWireFixture serves TWO artifacts, in the SAME response, that differ
// on exactly the axis TestSyncReportsPinnedTrueOnlyForAnHonouredPin exists to
// check: idPinnedA carries no pinOverride (a pin held for it was served
// exactly), idPinnedB carries one (a pin held for it was overridden). Both
// need a local pin in that test's pins.json for the fixture to exercise
// reportedPinned's `a.PinOverride != ""` branch rather than its `!ok` one.
const pinnedWireFixture = `{"artifacts":[
	{"id":"` + idPinnedA + `","revision":3,"type":"skill","name":"pinned-a","content":"WIRE-A","oldestServable":1,"latest":3},
	{"id":"` + idPinnedB + `","revision":3,"type":"skill","name":"pinned-b","content":"WIRE-B","oldestServable":1,"latest":5,"pinOverride":"ahead"}
]}`

// mixedArtifacts is the standard fixture: two file-backed artifacts that land,
// one that collides with a file the developer wrote herself (served, never
// applied), and one rule that reaches a registered project. It spans both
// sources of the applied union, Reconcile and ReconcileRules, and one served
// artifact that must be missing from it.
const mixedArtifacts = `{"artifacts":[
	{"id":"` + idFmt + `","revision":4,"type":"skill","name":"fmt","content":"FMT"},
	{"id":"` + idRev + `","revision":7,"type":"subagent","name":"rev","content":"REV"},
	{"id":"` + idMine + `","revision":9,"type":"skill","name":"mine","content":"FROM-ORBEAT"},
	{"id":"` + idRule + `","revision":5,"type":"rule","name":"house","content":"RULES"}
]}`

// reportBody is the wire shape cmd/sync must produce. It is declared here,
// independently of internal/syncclient's own structs, so a rename of the JSON
// tags on either side shows up as a decode that finds nothing rather than as
// two files agreeing with each other about a contract the server never saw.
type reportBody struct {
	InstallID string `json:"installId"`
	Artifacts []struct {
		ArtifactID string `json:"artifactId"`
		Revision   int    `json:"revision"`
	} `json:"artifacts"`
}

type fakeAPI struct {
	srv *httptest.Server

	mu      sync.Mutex
	reports []reportBody
	// reportsRaw holds the raw JSON body of every successfully-decoded
	// deployment report POST, in the same order as reports. reportBody
	// deliberately declares no Pinned field (see its own doc comment), so it
	// cannot answer "was the pinned key present at all, and with what
	// value": the exact question TestSyncReportsPinnedTrueOnlyForAnHonouredPin
	// exists to ask. Captured only on the path that already appends to
	// reports, so the two slices never desync.
	reportsRaw     [][]byte
	configRequests int
	// artifactsQueries records the raw query string (r.URL.RawQuery) of every
	// GET /v1/sync/artifacts request, in order. This is what makes the
	// difference between "FetchArtifacts builds the right URL" (asserted at
	// the internal/syncclient level) and "runSync actually hands it the
	// pins" observable: nothing in this fixture recorded the query before,
	// so a run that silently withheld every held pin from a pinning-capable
	// server passed every test in this file with the feature dead.
	artifactsQueries []string

	// registry is what GET /v1/sync/config advertises for deploymentRegistry.
	registry bool
	// pinning is what GET /v1/sync/config advertises for pinning (Task 6).
	pinning bool
	// reportStatus overrides the report route's response status (0 means 200).
	reportStatus int
	// configStatus overrides the config route's response status (0 means
	// 200), so a test can simulate GET /v1/sync/config itself failing,
	// distinct from reportStatus which only affects the deployments POST.
	configStatus int
	artifacts    string

	// strictDeployments makes the report route decode with
	// DisallowUnknownFields against reportBody, the shape THIS file declares
	// independently of internal/syncclient/api.go (its own doc comment says
	// why): a shape with no Pinned field, which is exactly what an
	// orbeat-api built before Task 7 landed decodes with. It stands in for
	// the skew trap this file's TestSyncNeverSendsPinnedUnlessConfigSaysSo
	// gate exists to catch: a client that sent "pinned" to a server whose
	// own deploymentReportEntry has no such field gets a 400 on the WHOLE
	// report from THAT server's real decodeJSON, and this fake reproduces
	// the same failure mode from the same cause (an unrecognised key),
	// without needing a second orbeat-api build to prove it.
	strictDeployments bool
}

func newFakeAPI(t *testing.T, artifacts string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{registry: true, artifacts: artifacts}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer AT" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sync/artifacts":
			f.mu.Lock()
			f.artifactsQueries = append(f.artifactsQueries, r.URL.RawQuery)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, f.artifacts)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sync/config":
			f.mu.Lock()
			f.configRequests++
			f.mu.Unlock()
			if f.configStatus != 0 {
				w.WriteHeader(f.configStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"gateway_url":        "http://gateway.invalid/mcp",
				"deploymentRegistry": f.registry,
				"pinning":            f.pinning,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sync/deployments":
			raw, _ := io.ReadAll(r.Body)
			var b reportBody
			var decodeErr error
			if f.strictDeployments {
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.DisallowUnknownFields()
				decodeErr = dec.Decode(&b)
			} else {
				decodeErr = json.Unmarshal(raw, &b)
			}
			if f.strictDeployments && decodeErr != nil {
				// The exact status a real decodeJSON returns on an unknown
				// field (internal/api's decodeJSONOrFail): the WHOLE report
				// is rejected, not the one field.
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if decodeErr != nil {
				t.Errorf("report body does not decode: %v (raw %s)", decodeErr, raw)
			}
			f.mu.Lock()
			f.reports = append(f.reports, b)
			f.reportsRaw = append(f.reportsRaw, raw)
			f.mu.Unlock()
			if f.reportStatus != 0 {
				w.WriteHeader(f.reportStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"recorded": len(b.Artifacts), "dropped": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) filed() []reportBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]reportBody(nil), f.reports...)
}

// reportsRawSeen returns the raw JSON body of every successfully-decoded
// deployment report POST, in order. Coordinator review finding on 425f6bc:
// the field-boundary gates (internal/syncclient's
// TestReportDeploymentsSendsPinnedOnlyWhenTrue, handed a pinned map
// directly; internal/api's TestDeploymentReportRecordsPinnedFromTheBody,
// handed a body that already carries "pinned") proved the encoder and the
// decoder each in isolation, but nothing drove a REAL `sync` and read what
// actually left the machine. `reportedPinned` mutated to `return nil`
// unconditionally passed every gate in this file for exactly that reason.
// This is what lets a test
// read the real wire instead of a value the test handed to a function
// itself.
func (f *fakeAPI) reportsRawSeen() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.reportsRaw...)
}

// configHits returns how many times GET /v1/sync/config has been called
// against this fake server so far.
func (f *fakeAPI) configHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configRequests
}

// artifactsQueriesSeen returns the raw query string of every GET
// /v1/sync/artifacts request seen so far, in order.
func (f *fakeAPI) artifactsQueriesSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.artifactsQueries...)
}

// rawOutcome decodes only what these gates assert, keeping the sections raw so
// a test can ask which KEYS a section carries and not only their values.
type rawOutcome struct {
	ExitCode  int             `json:"exitCode"`
	Artifacts json.RawMessage `json:"artifacts"`
	Seeds     json.RawMessage `json:"seeds"`
	Rules     json.RawMessage `json:"rules"`
	Report    json.RawMessage `json:"report"`
}

// syncFixture prepares an isolated HOME with a logged-in token, one registered
// project, and a hand-written unmanaged file that the "mine" artifact collides
// with. It returns the home directory.
func syncFixture(t *testing.T, f *fakeAPI) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home) // os.UserHomeDir honors $HOME on unix

	tokPath, err := syncclient.DefaultTokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := syncclient.SaveToken(tokPath, syncclient.Token{
		AccessToken: "AT", RefreshToken: "RT", Expiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	pj, err := syncclient.DefaultProjectsPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncclient.AddProject(pj, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}

	// Unmanaged, hand-authored, colliding with the "mine" skill: Reconcile
	// refuses to clobber it, so that artifact is served and never applied.
	mine := filepath.Join(home, ".claude", "skills", "mine", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("HAND-MADE"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// runSyncJSON runs the real `sync --json` command through dispatch and returns
// the decoded outcome plus the command error, so a gate can assert the process
// exit code the contract promises rather than only the JSON's own field.
func runSyncJSON(t *testing.T, f *fakeAPI) (rawOutcome, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"sync", "--json"})
	})
	var o rawOutcome
	if jerr := json.Unmarshal([]byte(out), &o); jerr != nil {
		t.Fatalf("sync --json did not emit one JSON object: %v\nstdout=%s", jerr, out)
	}
	return o, err
}

func decodeSection(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		t.Fatalf("section is null, want an object")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("section does not decode: %v (raw %s)", err, raw)
	}
	return m
}

func sectionCount(t *testing.T, raw json.RawMessage, key string) int {
	t.Helper()
	v, ok := decodeSection(t, raw)[key].(float64)
	if !ok {
		t.Fatalf("section has no numeric %q: %s", key, raw)
	}
	return int(v)
}

func sectionList(t *testing.T, raw json.RawMessage, key string) []any {
	t.Helper()
	v, ok := decodeSection(t, raw)[key].([]any)
	if !ok {
		t.Fatalf("section has no %q array: %s", key, raw)
	}
	return v
}

// THE LOAD-BEARING GATE: the body says what landed on disk, at the revision it
// landed at, and nothing else.
//
// It fails on three separate defects, each of which passes a "was the endpoint
// called" test: reporting the served set (idMine appears), reporting a
// constant revision (the numbers are all different and none is 1), and
// dropping the rules half of the union (idRule disappears).
func TestSyncReportsAppliedNotServed(t *testing.T) {
	f := newFakeAPI(t, mixedArtifacts)
	home := syncFixture(t, f)

	o, err := runSyncJSON(t, f)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if o.ExitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", o.ExitCode)
	}

	// The fixture must actually be doing what it claims: without this, a
	// reconciler that silently applied nothing would make the membership
	// assertions below pass for the wrong reason.
	if got := sectionCount(t, o.Artifacts, "added"); got != 2 {
		t.Fatalf("artifacts.added = %d, want 2 (fmt and rev landed, mine collided)", got)
	}
	if got := len(sectionList(t, o.Artifacts, "skipped")); got != 1 {
		t.Fatalf("artifacts.skipped = %v, want exactly the collided file", sectionList(t, o.Artifacts, "skipped"))
	}

	filed := f.filed()
	if len(filed) != 1 {
		t.Fatalf("the server received %d report(s), want exactly 1", len(filed))
	}
	rep := filed[0]

	// The install id on the wire is the one on disk, not an ad-hoc value.
	ipath := filepath.Join(home, ".config", "orbeat", "install.json")
	onDisk, err := syncclient.LoadInstallID(ipath)
	if err != nil {
		t.Fatalf("read install id: %v", err)
	}
	if onDisk == "" {
		t.Fatalf("the first report did not create %s", ipath)
	}
	if rep.InstallID != onDisk {
		t.Fatalf("reported installId %q, but %s holds %q", rep.InstallID, ipath, onDisk)
	}

	// Exact sequence, both directions: Reconcile sorts by artifact id and the
	// rules pass appends in name order, so this is deterministic. A subset or
	// a length check would pass on the served-set bug.
	want := []struct {
		id  string
		rev int
	}{{idFmt, 4}, {idRev, 7}, {idRule, 5}}
	if len(rep.Artifacts) != len(want) {
		t.Fatalf("reported %+v, want exactly %+v", rep.Artifacts, want)
	}
	for i, w := range want {
		if rep.Artifacts[i].ArtifactID != w.id || rep.Artifacts[i].Revision != w.rev {
			t.Fatalf("reported[%d] = %+v, want {%s %d} (full: %+v)", i, rep.Artifacts[i], w.id, w.rev, rep.Artifacts)
		}
	}
	for _, a := range rep.Artifacts {
		if a.ArtifactID == idMine {
			t.Fatalf("the collided artifact was reported as applied: %+v", rep.Artifacts)
		}
	}

	repSec := decodeSection(t, o.Report)
	if repSec["recorded"] != float64(3) {
		t.Fatalf("report.recorded = %v, want 3", repSec["recorded"])
	}
	if w, ok := repSec["warnings"].([]any); !ok || len(w) != 0 {
		t.Fatalf("report.warnings = %v, want an empty array on a clean report", repSec["warnings"])
	}
}

// G5: a report that does not arrive is a Warning at exit 0, never a Failure.
//
// Both halves matter and they fail to different mutants: the exit code catches
// a run that maps the failure into the retry contract, and the empty Failures
// lists catch one that files it as a unit that should have synced. The
// artifacts counters catch a run that abandoned the sync over a bookkeeping
// POST.
func TestSyncFailedReportIsAWarningAtExitZero(t *testing.T) {
	f := newFakeAPI(t, mixedArtifacts)
	f.reportStatus = http.StatusInternalServerError
	syncFixture(t, f)

	o, err := runSyncJSON(t, f)
	if err != nil {
		t.Fatalf("a failed report must not fail the command, got err %v", err)
	}
	if rc := exitCode(err); rc != 0 {
		t.Fatalf("process exit code = %d, want 0 (a bookkeeping POST must not arm a cron retry)", rc)
	}
	if o.ExitCode != 0 {
		t.Fatalf("outcome.exitCode = %d, want 0", o.ExitCode)
	}

	// The files really did sync, which is why exit 0 is the honest answer.
	if got := sectionCount(t, o.Artifacts, "added"); got != 2 {
		t.Fatalf("artifacts.added = %d, want 2", got)
	}
	for name, raw := range map[string]json.RawMessage{"artifacts": o.Artifacts, "seeds": o.Seeds, "rules": o.Rules} {
		if fails := sectionList(t, raw, "failures"); len(fails) != 0 {
			t.Fatalf("%s.failures = %v, want empty: a failed report is not a unit that failed to sync", name, fails)
		}
	}

	if s := string(o.Report); s == "null" {
		t.Fatal("the report section is null after a 500: a swallowed failure is indistinguishable from a stage that never ran")
	}
	rep := decodeSection(t, o.Report)
	warnings, _ := rep["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatalf("report.warnings is empty: a failed report must say so, not vanish (report=%s)", o.Report)
	}
	// The taxonomy expressed in the type: there is no field a report failure
	// could be filed under, so a consumer cannot find one.
	if _, ok := rep["failures"]; ok {
		t.Fatalf("the report section carries a failures field (%s): a report failure must have no shape that reaches the exit code", o.Report)
	}
	if rep["recorded"] != float64(0) {
		t.Fatalf("report.recorded = %v, want 0 on a failed report", rep["recorded"])
	}
}

// A fatal run sends nothing. A report is a REPLACE, and on the corrupt-manifest
// path Reconcile returns before its write loop runs at all, so the applied set
// is empty for a reason that has nothing to do with what is on disk: reporting
// it would delete this install's rows and read exactly like a developer whose
// entitlements were all revoked.
func TestSyncFatalRunReportsNothing(t *testing.T) {
	f := newFakeAPI(t, mixedArtifacts)
	home := syncFixture(t, f)
	if err := os.WriteFile(filepath.Join(home, ".claude", ".orbeat-sync-manifest.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	o, err := runSyncJSON(t, f)
	if rc := exitCode(err); rc != 2 {
		t.Fatalf("exit code = %d, want 2 (the fixture must actually abort, or this gate proves nothing)", rc)
	}
	if o.ExitCode != 2 {
		t.Fatalf("outcome.exitCode = %d, want 2", o.ExitCode)
	}
	if n := len(f.filed()); n != 0 {
		t.Fatalf("a fatal run filed %d report(s), want 0", n)
	}
	if s := string(o.Report); s != "null" {
		t.Fatalf("report section = %s, want null (the stage never ran)", s)
	}
}

// A server that does not advertise the registry is never posted to. This is
// the Community path: internal/syncclient and cmd/sync ship in both editions,
// and a generated Community tree registers no report route at all, so the
// advertised flag is the only thing standing between this client and a 404 on
// every sync.
func TestSyncDoesNotReportWhenServerDoesNotAdvertiseIt(t *testing.T) {
	f := newFakeAPI(t, mixedArtifacts)
	f.registry = false
	home := syncFixture(t, f)

	o, err := runSyncJSON(t, f)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n := len(f.filed()); n != 0 {
		t.Fatalf("filed %d report(s) against a server that does not record them, want 0", n)
	}
	if s := string(o.Report); s != "null" {
		t.Fatalf("report section = %s, want null", s)
	}
	// And no install identity was written: a client that never reports leaves
	// no machine id on disk.
	if _, err := os.Stat(filepath.Join(home, ".config", "orbeat", "install.json")); !os.IsNotExist(err) {
		t.Fatalf("install.json exists after a run that reported nothing (stat err %v)", err)
	}
}

// A server that serves artifacts it does not identify is not reported to
// either. appendApplied drops an id-less artifact because there is no key a
// row could be stored under, which leaves the applied set short by exactly
// those artifacts, and a replace built from it deletes their rows: a
// de-entitlement that never happened.
func TestSyncDoesNotReportWhenServedArtifactsAreUnidentified(t *testing.T) {
	f := newFakeAPI(t, `{"artifacts":[
		{"revision":4,"type":"skill","name":"fmt","content":"FMT"},
		{"id":"`+idRev+`","revision":7,"type":"subagent","name":"rev","content":"REV"}
	]}`)
	syncFixture(t, f)

	o, err := runSyncJSON(t, f)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := sectionCount(t, o.Artifacts, "added"); got != 2 {
		t.Fatalf("artifacts.added = %d, want 2: the files still sync, only the report is withheld", got)
	}
	if n := len(f.filed()); n != 0 {
		t.Fatalf("filed %d report(s) built from artifacts the server did not name, want 0", n)
	}
	if len(sectionList(t, o.Report, "warnings")) == 0 {
		t.Fatalf("withholding a report must say so: report=%s", o.Report)
	}
}

// The install id is written once and reused. A client that regenerated it per
// run would show one developer's single machine as a new install on every
// sync, and the old rows would linger until retention pruned them.
func TestSyncInstallIDIsCreatedOnceAndReused(t *testing.T) {
	f := newFakeAPI(t, mixedArtifacts)
	home := syncFixture(t, f)
	ipath := filepath.Join(home, ".config", "orbeat", "install.json")

	if _, err := os.Stat(ipath); !os.IsNotExist(err) {
		t.Fatalf("install.json exists before the first report (stat err %v)", err)
	}
	if _, err := runSyncJSON(t, f); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	first, err := os.ReadFile(ipath)
	if err != nil {
		t.Fatalf("the first report did not create install.json: %v", err)
	}
	if _, err := runSyncJSON(t, f); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	second, err := os.ReadFile(ipath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("install.json changed between runs: %q then %q", first, second)
	}

	filed := f.filed()
	if len(filed) != 2 {
		t.Fatalf("filed %d report(s) over two runs, want 2", len(filed))
	}
	if filed[0].InstallID != filed[1].InstallID {
		t.Fatalf("two runs from one machine reported different install ids: %q then %q",
			filed[0].InstallID, filed[1].InstallID)
	}
	if filed[0].InstallID == "" {
		t.Fatal("reported an empty install id")
	}
}

// A dry run reports nothing. Applied names what a real run WOULD apply, and a
// report built from a plan would record files that are not on disk.
func TestSyncDryRunReportsNothing(t *testing.T) {
	f := newFakeAPI(t, mixedArtifacts)
	home := syncFixture(t, f)

	var err error
	out := captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL},
			[]string{"sync", "--dry-run", "--json"})
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	var o rawOutcome
	if jerr := json.Unmarshal([]byte(out), &o); jerr != nil {
		t.Fatalf("sync --dry-run --json did not emit one JSON object: %v\nstdout=%s", jerr, out)
	}
	// The fixture must actually have planned something, or the assertions
	// below hold for a run that did nothing at all.
	if got := sectionCount(t, o.Artifacts, "added"); got != 2 {
		t.Fatalf("artifacts.added = %d, want 2 planned adds", got)
	}
	if n := len(f.filed()); n != 0 {
		t.Fatalf("a dry run filed %d report(s), want 0", n)
	}
	if s := string(o.Report); s != "null" {
		t.Fatalf("report section = %s, want null", s)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "orbeat", "install.json")); !os.IsNotExist(err) {
		t.Fatalf("a dry run wrote an install identity (stat err %v)", err)
	}
}

// An exit-1 run DOES report, and it reports only the units that landed.
//
// Per-unit isolation (v1.15.0) exists so one unwritable file does not discard
// a whole sync; suppressing the report on that path would throw away true
// facts about every artifact that did land, because of an unrelated failure.
// The artifact whose write failed must be absent all the same: its previous
// bytes are still on disk, so claiming the served revision would be exactly
// the falsehood the applied-not-requested rule exists to prevent.
func TestSyncPartialRunStillReportsWhatLanded(t *testing.T) {
	f := newFakeAPI(t, mixedArtifacts)
	home := syncFixture(t, f)

	// Make one target's parent directory unwritable, so writeFileAtomic's temp
	// file cannot be created there and Reconcile records a non-fatal failure.
	blocked := filepath.Join(home, ".claude", "skills", "fmt")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	// Registered after t.TempDir's own cleanup, so it runs BEFORE it (LIFO)
	// and the temp tree can still be removed.
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	o, err := runSyncJSON(t, f)
	if rc := exitCode(err); rc != 1 {
		t.Fatalf("exit code = %d, want 1 (the fixture must actually fail one unit, or this gate proves nothing; err %v)", rc, err)
	}
	if o.ExitCode != 1 {
		t.Fatalf("outcome.exitCode = %d, want 1", o.ExitCode)
	}
	if fails := sectionList(t, o.Artifacts, "failures"); len(fails) != 1 {
		t.Fatalf("artifacts.failures = %v, want exactly the blocked write", fails)
	}

	filed := f.filed()
	if len(filed) != 1 {
		t.Fatalf("a partial run filed %d report(s), want 1: the healthy units really did sync", len(filed))
	}
	want := []struct {
		id  string
		rev int
	}{{idRev, 7}, {idRule, 5}}
	if len(filed[0].Artifacts) != len(want) {
		t.Fatalf("reported %+v, want exactly %+v", filed[0].Artifacts, want)
	}
	for i, w := range want {
		if filed[0].Artifacts[i].ArtifactID != w.id || filed[0].Artifacts[i].Revision != w.rev {
			t.Fatalf("reported[%d] = %+v, want {%s %d}", i, filed[0].Artifacts[i], w.id, w.rev)
		}
	}
}

// The gates below cover the pinning slice's client half (artifact-version-
// pinning plan, Task 6): the capability negotiation a `sync` performs
// against a server that cannot honour a held pin, the /v1/sync/config
// ordering change (one fetch per run, in every shape a run can take), and
// the `pin`/`pin remove`/`pin list` round trip plus the run lock protecting
// every mutating form.

// Gate 9: a client holding a pin against a server reporting pinning:false
// must warn about it, still sync whatever the server actually serves, exit
// 0, and leave pins.json untouched. Silently dropping the pin (serving
// latest with no warning at all) is exactly the skew defect the capability
// negotiation exists to prevent, and treating the override as a Failure
// (exit 1) would arm a retry loop forever: nothing about the next run would
// ever be different.
func TestSyncWarnsOnAPinTheServerCannotHonour(t *testing.T) {
	f := newFakeAPI(t, pinnableArtifact)
	f.pinning = false
	home := syncFixture(t, f)

	pinsPath := filepath.Join(home, ".config", "orbeat", "pins.json")
	if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: idPin, Type: "skill", Name: "pinme", Revision: 1}); err != nil {
		t.Fatalf("seed pin: %v", err)
	}
	before, err := os.ReadFile(pinsPath)
	if err != nil {
		t.Fatalf("read pins.json before sync: %v", err)
	}

	o, err := runSyncJSON(t, f)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if o.ExitCode != 0 {
		t.Fatalf("exitCode = %d, want 0 (an overridden pin is a warning, never a failure)", o.ExitCode)
	}
	if got := sectionCount(t, o.Artifacts, "added"); got != 1 {
		t.Fatalf("artifacts.added = %d, want 1 (the artifact still syncs, at whatever the server serves)", got)
	}

	pins := sectionList(t, o.Artifacts, "pins")
	if len(pins) != 1 {
		t.Fatalf("artifacts.pins = %v, want exactly one overridden pin", pins)
	}
	entry, ok := pins[0].(map[string]any)
	if !ok || entry["requested"] != float64(1) || entry["served"] != float64(3) || entry["reason"] != "unsupported" {
		t.Fatalf("pins[0] = %v, want requested=1 served=3 reason=unsupported", pins[0])
	}

	if warnings := sectionList(t, o.Artifacts, "warnings"); len(warnings) == 0 {
		t.Fatal("artifacts.warnings is empty: a pin the server cannot honour must say so")
	}

	after, err := os.ReadFile(pinsPath)
	if err != nil {
		t.Fatalf("read pins.json after sync: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("sync rewrote pins.json: before=%s after=%s", before, after)
	}

	// THE DIRECT OBSERVATION, not an inference from the warning above: a
	// server that does not support pinning must receive a request with NO
	// pin parameter at all. A client that built the warning from
	// pinOutcomeUnsupported but sent ?pin= anyway (harmless against THIS
	// fake server, since it ignores the query, but a live server would
	// silently apply a pin it should never have received) would still pass
	// every assertion above.
	queries := f.artifactsQueriesSeen()
	if len(queries) != 1 {
		t.Fatalf("GET /v1/sync/artifacts called %d time(s), want exactly 1", len(queries))
	}
	if strings.Contains(queries[0], "pin=") {
		t.Fatalf("query = %q, want no pin parameter at all against a server that does not support pinning", queries[0])
	}
}

// Gate (review finding on 15f41f3): the capability negotiation is dead if
// runSync computes the right ?pin= query but never actually sends it, or
// sends it to a server that never asked for it. Every existing gate at this
// level inferred "the pin was sent" from a WARNING or from pins.json being
// left alone, and both of those are satisfied by a client that never sends
// ?pin= to ANY server: newFakeAPI's own /v1/sync/artifacts handler did not
// even record the query it received. This test observes the request itself.
//
// Mutant this catches: `sendPins, forcedReason := pins, ""` rewritten to
// `sendPins, forcedReason := []syncclient.Pin(nil), ""` in runSync, which
// withholds every pin from every server, including one advertising
// pinning:true, with forcedReason staying "" so no warning fires either,
// the silent version of the exact failure the capability negotiation exists
// to prevent.
func TestSyncSendsPinToAServerThatSupportsIt(t *testing.T) {
	f := newFakeAPI(t, pinnableArtifact)
	f.pinning = true
	home := syncFixture(t, f)

	pinsPath := filepath.Join(home, ".config", "orbeat", "pins.json")
	if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: idPin, Type: "skill", Name: "pinme", Revision: 1}); err != nil {
		t.Fatalf("seed pin: %v", err)
	}

	if _, err := runSyncJSON(t, f); err != nil {
		t.Fatalf("sync: %v", err)
	}

	queries := f.artifactsQueriesSeen()
	if len(queries) != 1 {
		t.Fatalf("GET /v1/sync/artifacts called %d time(s), want exactly 1", len(queries))
	}
	q, err := url.ParseQuery(queries[0])
	if err != nil {
		t.Fatalf("query %q does not parse: %v", queries[0], err)
	}
	want := idPin + ":1"
	got := q["pin"]
	if len(got) != 1 || got[0] != want {
		t.Fatalf("pin params = %v, want exactly [%q] (raw query %q)", got, want, queries[0])
	}
}

// Gate (review finding on 15f41f3, applied to a sibling branch): the
// scfgErr != nil switch case (a config-fetch failure) was wired in runSync
// and threaded into reportInput, but nothing exercised it through a real
// sync before this test. Every existing gate at this level either used a
// healthy config fetch, or the registry:false / pinning:false branches,
// which are DIFFERENT code paths (a nil scfgErr, a real SyncConfig with one
// field false) from an actual fetch failure (a non-nil scfgErr, a zero
// SyncConfig). This drives GET /v1/sync/config itself to fail (500) and
// asserts every consequence directly: no pin parameter reaches the wire, the
// artifacts section carries its own separate withheld-pin warning
// (reason=unknown), and the report section still carries its pre-existing
// warning, told apart rather than merged into one sentence (see
// reportDeployments' own doc comment on this exact point).
func TestSyncWarnsOnBothConsequencesWhenConfigFetchFails(t *testing.T) {
	f := newFakeAPI(t, pinnableArtifact)
	f.configStatus = http.StatusInternalServerError
	home := syncFixture(t, f)

	pinsPath := filepath.Join(home, ".config", "orbeat", "pins.json")
	if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: idPin, Type: "skill", Name: "pinme", Revision: 1}); err != nil {
		t.Fatalf("seed pin: %v", err)
	}

	o, err := runSyncJSON(t, f)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if o.ExitCode != 0 {
		t.Fatalf("exitCode = %d, want 0 (a config-fetch failure must not abort the sync)", o.ExitCode)
	}

	queries := f.artifactsQueriesSeen()
	if len(queries) != 1 {
		t.Fatalf("GET /v1/sync/artifacts called %d time(s), want exactly 1", len(queries))
	}
	if strings.Contains(queries[0], "pin=") {
		t.Fatalf("query = %q, want no pin parameter when the config fetch itself failed", queries[0])
	}

	pins := sectionList(t, o.Artifacts, "pins")
	if len(pins) != 1 {
		t.Fatalf("artifacts.pins = %v, want exactly one withheld pin", pins)
	}
	if entry, ok := pins[0].(map[string]any); !ok || entry["reason"] != "unknown" {
		t.Fatalf("pins[0] = %v, want reason=unknown", pins[0])
	}
	if warnings := sectionList(t, o.Artifacts, "warnings"); len(warnings) == 0 {
		t.Fatal("artifacts.warnings is empty: a withheld pin must say so")
	}

	if s := string(o.Report); s == "null" {
		t.Fatal("report section is null: a config-fetch failure must still produce a report Warning, not a swallowed stage")
	}
	rep := decodeSection(t, o.Report)
	repWarnings, _ := rep["warnings"].([]any)
	if len(repWarnings) == 0 {
		t.Fatal("report.warnings is empty: a config-fetch failure must say the report was withheld")
	}
}

// Gate: a --dry-run must plan WITH the pins applied, per the ordering
// hoist's own stated requirement, not silently plan against the unpinned
// latest because the fetch happens to run before the dry-run branch is
// checked. Direct observation of the wire query, same discipline as the two
// gates above.
func TestSyncDryRunSendsPinToo(t *testing.T) {
	f := newFakeAPI(t, pinnableArtifact)
	f.pinning = true
	home := syncFixture(t, f)

	pinsPath := filepath.Join(home, ".config", "orbeat", "pins.json")
	if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: idPin, Type: "skill", Name: "pinme", Revision: 1}); err != nil {
		t.Fatalf("seed pin: %v", err)
	}

	var err error
	captureStdout(t, func() {
		err = dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"sync", "--dry-run", "--json"})
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}

	queries := f.artifactsQueriesSeen()
	if len(queries) != 1 {
		t.Fatalf("GET /v1/sync/artifacts called %d time(s), want exactly 1", len(queries))
	}
	q, err := url.ParseQuery(queries[0])
	if err != nil {
		t.Fatalf("query %q does not parse: %v", queries[0], err)
	}
	want := idPin + ":1"
	if got := q["pin"]; len(got) != 1 || got[0] != want {
		t.Fatalf("pin params = %v, want exactly [%q] (raw query %q)", got, want, queries[0])
	}
}

// TestSyncNeverSendsPinnedUnlessConfigSaysSo is Task 7's skew-trap gate. A
// client that computes "pinned" for the deployment report must never send it
// to a server that has not, THIS RUN, confirmed it understands the key. A
// server with the registry on but pinning off is exactly that server: GET
// /v1/sync/config answers pinning:false, so this run must withhold "pinned"
// entirely: its own deploymentReportEntry would 400 the WHOLE report on the
// unrecognised key, and strictDeployments reproduces that decoder exactly
// (DisallowUnknownFields against a shape with no Pinned field, the same shape
// an orbeat-api built before this task decodes with).
//
// Red-proven by making reportedPinned ignore its supported argument (compute
// the map from pins/served alone, dropping the `if !supported` guard):
// pinnableArtifact never sets pinOverride, so the held pin below reads as
// "honoured" regardless of whether pinning was ever confirmed, and the client
// sends pinned:true unconditionally. Against THIS fake that turns into a 400
// on the whole report: report.warnings becomes non-empty, f.filed() stays
// empty (the strict decoder never got past the unknown key to append
// anything), and report.recorded reads its zero value, while the run still
// exits 0, which is precisely the silent failure the two defences exist to
// prevent.
func TestSyncNeverSendsPinnedUnlessConfigSaysSo(t *testing.T) {
	f := newFakeAPI(t, pinnableArtifact)
	f.pinning = false
	f.strictDeployments = true
	home := syncFixture(t, f)

	pinsPath := filepath.Join(home, ".config", "orbeat", "pins.json")
	if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: idPin, Type: "skill", Name: "pinme", Revision: 3}); err != nil {
		t.Fatalf("seed pin: %v", err)
	}

	o, err := runSyncJSON(t, f)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if o.ExitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", o.ExitCode)
	}

	rep := decodeSection(t, o.Report)
	repWarnings, _ := rep["warnings"].([]any)
	if len(repWarnings) != 0 {
		t.Fatalf("report.warnings = %v, want none: a server with pinning:false must never receive "+
			"\"pinned\" at all, so its strict decoder must never see the unrecognised key (raw %s)", repWarnings, o.Report)
	}
	if recorded, _ := rep["recorded"].(float64); recorded != 1 {
		t.Fatalf("report.recorded = %v, want 1: the report must have actually reached and been accepted "+
			"by the server (raw %s)", rep["recorded"], o.Report)
	}

	if filed := f.filed(); len(filed) != 1 {
		t.Fatalf("fake server recorded %d report(s), want exactly 1: a strict-decode failure means the "+
			"request never got past the unrecognised key", len(filed))
	}
}

// TestSyncReportsPinnedTrueOnlyForAnHonouredPin is the end-to-end proof
// neither of Task 7's two field-boundary gates can give.
//
// internal/syncclient's TestReportDeploymentsSendsPinnedOnlyWhenTrue calls
// ReportDeployments directly with a pinned map the TEST built by hand: it
// proves the ENCODER (map -> JSON on the wire), never how that map gets
// computed. internal/api's TestDeploymentReportRecordsPinnedFromTheBody
// posts a body the TEST built by hand, already carrying "pinned": it proves
// the DECODER and the column, never what a real client would have sent.
// Both ends were gated and the wire between them, cmd/sync's own
// reportedPinned, was not.
//
// Coordinator review finding on 425f6bc: `reportedPinned` mutated to
// `return nil` unconditionally (the first line of its body, before the
// `!supported` check even runs) passed every gate that existed in this file,
// TestSyncNeverSendsPinnedUnlessConfigSaysSo included, because that gate
// asserts the pinning:false branch, where sending nothing IS correct. That
// is the exact seam Task 6's own review found (a fake server that never recorded
// the query it received let a `runSync` sending no pins at all through with
// 59 tests green). This test is that lesson applied one field over: it
// drives a REAL `sync` through dispatch against a server advertising
// pinning:true, holding a pin the server actually honours, and reads the
// POST body that left the machine on the real wire (fakeAPI.reportsRawSeen),
// not a value any test handed to a function itself.
//
// Two artifacts in the SAME report, both with a local pin held for them,
// kill two different mutants: idPinnedA (no pinOverride, pin honoured
// exactly) must carry a literal "pinned":true, which `return nil` fails to
// produce; idPinnedB (pinOverride:"ahead", pin overridden) must carry NO
// "pinned" key at all, which a mutant that ignores PinOverride and marks
// every held pin true would fail to omit.
func TestSyncReportsPinnedTrueOnlyForAnHonouredPin(t *testing.T) {
	f := newFakeAPI(t, pinnedWireFixture)
	f.pinning = true
	home := syncFixture(t, f)

	pinsPath := filepath.Join(home, ".config", "orbeat", "pins.json")
	if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: idPinnedA, Type: "skill", Name: "pinned-a", Revision: 3}); err != nil {
		t.Fatalf("seed pin a: %v", err)
	}
	if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: idPinnedB, Type: "skill", Name: "pinned-b", Revision: 1}); err != nil {
		t.Fatalf("seed pin b: %v", err)
	}

	if _, err := runSyncJSON(t, f); err != nil {
		t.Fatalf("sync: %v", err)
	}

	raws := f.reportsRawSeen()
	if len(raws) != 1 {
		t.Fatalf("fake server recorded %d report(s), want exactly 1", len(raws))
	}

	// A LOCAL decode target, independent of reportBody (which deliberately
	// has no Pinned field) and of syncclient's own deploymentReportItem: a
	// rename on either side of the wire must show up here as a key this
	// struct cannot find, not as two files agreeing about each other.
	// Pinned is a pointer so nil means "absent from the wire" and a non-nil
	// zero value means "present and false", two different facts an
	// ordinary bool cannot tell apart.
	var wire struct {
		InstallID string `json:"installId"`
		Artifacts []struct {
			ArtifactID string `json:"artifactId"`
			Revision   int    `json:"revision"`
			Pinned     *bool  `json:"pinned"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(raws[0], &wire); err != nil {
		t.Fatalf("report body does not decode: %v (raw %s)", err, raws[0])
	}
	byID := make(map[string]*bool, len(wire.Artifacts))
	for _, a := range wire.Artifacts {
		byID[a.ArtifactID] = a.Pinned
	}

	got, ok := byID[idPinnedA]
	if !ok {
		t.Fatalf("idPinnedA is missing from the report entirely (raw %s)", raws[0])
	}
	if got == nil || !*got {
		t.Fatalf("idPinnedA's \"pinned\" = %v, want a literal true on the wire: it has a pin held for it "+
			"and the server served it exactly (no pinOverride) (raw %s)", got, raws[0])
	}

	got, ok = byID[idPinnedB]
	if !ok {
		t.Fatalf("idPinnedB is missing from the report entirely (raw %s)", raws[0])
	}
	if got != nil {
		t.Fatalf("idPinnedB carries \"pinned\":%v, want the key ABSENT entirely: its pin was overridden "+
			"(pinOverride:\"ahead\"), so pinned is false, and omitempty must keep an ordinary false off "+
			"the wire (raw %s)", *got, raws[0])
	}
}

// Gate (Task 6): the ordering change that hoists the /v1/sync/config fetch
// above the artifact fetch must not turn one round trip into two, and must
// not turn a dry run or a fatal abort into an accidental deployment POST.
// All three run shapes fetch the config document exactly once; only a real,
// non-fatal run ever posts.
func TestSyncConfigFetchedExactlyOncePerRun(t *testing.T) {
	t.Run("real run", func(t *testing.T) {
		f := newFakeAPI(t, mixedArtifacts)
		syncFixture(t, f)
		if _, err := runSyncJSON(t, f); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if got := f.configHits(); got != 1 {
			t.Fatalf("GET /v1/sync/config called %d time(s), want exactly 1", got)
		}
	})

	t.Run("dry run", func(t *testing.T) {
		f := newFakeAPI(t, mixedArtifacts)
		syncFixture(t, f)
		var err error
		captureStdout(t, func() {
			err = dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"sync", "--dry-run", "--json"})
		})
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if got := f.configHits(); got != 1 {
			t.Fatalf("GET /v1/sync/config called %d time(s) on a dry run, want exactly 1", got)
		}
		if n := len(f.filed()); n != 0 {
			t.Fatalf("a dry run posted %d deployment report(s), want 0", n)
		}
	})

	t.Run("fatal abort", func(t *testing.T) {
		f := newFakeAPI(t, mixedArtifacts)
		home := syncFixture(t, f)
		if err := os.WriteFile(filepath.Join(home, ".claude", ".orbeat-sync-manifest.json"), []byte("{bad"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := runSyncJSON(t, f)
		if rc := exitCode(err); rc != 2 {
			t.Fatalf("exit code = %d, want 2 (the fixture must actually abort, or this gate proves nothing)", rc)
		}
		if got := f.configHits(); got != 1 {
			t.Fatalf("GET /v1/sync/config called %d time(s) on a fatal abort, want exactly 1 (the fetch now precedes the reconcile)", got)
		}
		if n := len(f.filed()); n != 0 {
			t.Fatalf("a fatal abort posted %d deployment report(s), want 0", n)
		}
	})
}

// Gate 10: 'pin' writes a pin, 'pin remove' removes it, 'pin list' reads
// what's there, and a real sync (which never writes pins.json at all)
// leaves a still-held pin file byte-identical. The run-lock half of this
// gate (a mutating pin form refused while a run is in flight) lives beside
// its sibling in cmd/orbeat-sync/main_test.go's TestProjectAddRequiresRunLock,
// since it needs no fake server at all: the lock is taken before any
// network call.
func TestPinRoundTripAndSyncLeavesItAlone(t *testing.T) {
	f := newFakeAPI(t, pinnableArtifact)
	f.pinning = true
	home := syncFixture(t, f)
	pinsPath := filepath.Join(home, ".config", "orbeat", "pins.json")

	if err := dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"pin", "skill/pinme"}); err != nil {
		t.Fatalf("pin: %v", err)
	}
	pins, err := syncclient.LoadPins(pinsPath)
	if err != nil {
		t.Fatalf("load pins: %v", err)
	}
	if len(pins) != 1 || pins[0].ArtifactID != idPin || pins[0].Revision != 3 {
		t.Fatalf("pins = %+v, want one pin naming %s at revision 3", pins, idPin)
	}

	listed := captureStdout(t, func() {
		if err := dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"pin", "list"}); err != nil {
			t.Fatalf("pin list: %v", err)
		}
	})
	if !strings.Contains(listed, "skill/pinme") || !strings.Contains(listed, "3") {
		t.Fatalf("pin list output = %q, want it to name skill/pinme at revision 3", listed)
	}

	if err := dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"pin", "remove", "skill/pinme"}); err != nil {
		t.Fatalf("pin remove: %v", err)
	}
	if pins, err := syncclient.LoadPins(pinsPath); err != nil || len(pins) != 0 {
		t.Fatalf("pins after remove = %+v (err %v), want none", pins, err)
	}

	// Re-pin, DELIBERATELY at a revision (1) the fake server does not
	// actually serve (it always returns "V3"/revision 3, since it does not
	// implement the clamp): requested and served now differ, which is what
	// makes "sync leaves the file alone" non-vacuous against a mutant that
	// rewrites pins.json with the SERVED revision: if requested already
	// equalled served, such a mutant would happen to write the same bytes
	// back and this assertion would pass for the wrong reason.
	if err := dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"pin", "skill/pinme", "--revision", "1"}); err != nil {
		t.Fatalf("re-pin: %v", err)
	}
	if pins, err := syncclient.LoadPins(pinsPath); err != nil || len(pins) != 1 || pins[0].Revision != 1 {
		t.Fatalf("pins after re-pin = %+v (err %v), want one pin at revision 1", pins, err)
	}
	before, err := os.ReadFile(pinsPath)
	if err != nil {
		t.Fatalf("read pins.json: %v", err)
	}
	if _, err := runSyncJSON(t, f); err != nil {
		t.Fatalf("sync: %v", err)
	}
	after, err := os.ReadFile(pinsPath)
	if err != nil {
		t.Fatalf("read pins.json after sync: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("sync rewrote pins.json: before=%s after=%s", before, after)
	}
}

// --revision is validated against the response's oldestServable/latest
// before anything is written, so a typo'd number fails at pin time rather
// than surfacing as a silent "pruned" warning at the next sync.
func TestPinSetValidatesRevisionWindow(t *testing.T) {
	f := newFakeAPI(t, pinnableArtifact)
	f.pinning = true
	home := syncFixture(t, f)
	pinsPath := filepath.Join(home, ".config", "orbeat", "pins.json")

	if err := dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"pin", "skill/pinme", "--revision", "5"}); err == nil {
		t.Fatal("pin --revision 5 (above latest=3) succeeded, want an error")
	}
	if pins, _ := syncclient.LoadPins(pinsPath); len(pins) != 0 {
		t.Fatalf("a rejected --revision wrote a pin anyway: %+v", pins)
	}

	if err := dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"pin", "skill/pinme", "--revision", "2"}); err != nil {
		t.Fatalf("pin --revision 2 (within [1,3]): %v", err)
	}
	pins, err := syncclient.LoadPins(pinsPath)
	if err != nil || len(pins) != 1 || pins[0].Revision != 2 {
		t.Fatalf("pins = %+v (err %v), want one pin at revision 2", pins, err)
	}
}

// Gate (review finding on 15f41f3, applied to a sibling wiring point):
// runPinSet's OWN discovery fetch is supposed to carry every pin already
// held, not just the one being set, so a repeat 'pin' against an
// already-pinned artifact re-affirms the currently-served revision rather
// than the fetch silently reverting to what an unpinned request would
// return. pinnableArtifact's fixture body cannot distinguish "existing sent"
// from "existing withheld" through written content alone, since the fake
// server never applies a clamp, so this observes the wire query directly.
func TestPinSetSendsExistingPinsOnItsOwnDiscoveryFetch(t *testing.T) {
	f := newFakeAPI(t, pinnableArtifact)
	f.pinning = true
	home := syncFixture(t, f)
	pinsPath := filepath.Join(home, ".config", "orbeat", "pins.json")

	otherID := "66666666-6666-4666-8666-666666666666"
	if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: otherID, Type: "subagent", Name: "other", Revision: 2}); err != nil {
		t.Fatalf("seed existing pin: %v", err)
	}

	if err := dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"pin", "skill/pinme"}); err != nil {
		t.Fatalf("pin: %v", err)
	}

	queries := f.artifactsQueriesSeen()
	if len(queries) != 1 {
		t.Fatalf("GET /v1/sync/artifacts called %d time(s), want exactly 1", len(queries))
	}
	q, err := url.ParseQuery(queries[0])
	if err != nil {
		t.Fatalf("query %q does not parse: %v", queries[0], err)
	}
	want := otherID + ":2"
	found := false
	for _, g := range q["pin"] {
		if g == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("pin params = %v, want them to include the already-held pin %q (raw query %q)", q["pin"], want, queries[0])
	}
}

// 'pin <type>/<name>' against a server that has not advertised pinning must
// REFUSE, and the reproduction is why it cannot merely warn: the bare form
// pins "whatever this fetch served", and a server that never parses ?pin=
// serves head, so the write silently moves a held pin forward. Seeded at
// revision 1 against a fixture that serves revision 3, the pre-fix client
// printed "Pinned skill/pinme to revision 3" and rewrote pins.json to 3.
//
// This gate is the sibling of TestPinSetSendsExistingPinsOnItsOwnDiscoveryFetch
// and deliberately inverts the one field that test holds true: at
// pinning = true, runPinSet's own discovery fetch is honoured and the defect
// is invisible, which is exactly why the existing gate could never see it.
//
// The second subtest covers a config fetch that FAILS rather than one that
// answers false. Both mean the same thing to a write: this client has not been
// told the server keeps pins. runSync may degrade to "sync latest and warn"
// because it writes no pin file; a command whose entire product is a
// pins.json entry has nothing left to degrade to.
func TestPinSetRefusesWhenServerDoesNotAdvertisePinning(t *testing.T) {
	t.Run("server says pinning:false", func(t *testing.T) {
		f := newFakeAPI(t, pinnableArtifact)
		f.pinning = false
		home := syncFixture(t, f)
		pinsPath := filepath.Join(home, ".config", "orbeat", "pins.json")

		if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: idPin, Type: "skill", Name: "pinme", Revision: 1}); err != nil {
			t.Fatalf("seed existing pin: %v", err)
		}

		err := dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"pin", "skill/pinme"})
		if err == nil {
			t.Fatal("pin against a server that does not advertise pinning succeeded, want a refusal")
		}
		if !strings.Contains(err.Error(), "pinning") {
			t.Fatalf("refusal = %q, want it to name pinning as what the server lacks", err)
		}

		pins, lerr := syncclient.LoadPins(pinsPath)
		if lerr != nil {
			t.Fatalf("load pins: %v", lerr)
		}
		// The load-bearing assertion. A refusal that still wrote the pin
		// would leave the developer held at a revision she never chose,
		// which is the whole defect.
		if len(pins) != 1 || pins[0].Revision != 1 {
			t.Fatalf("pins = %+v, want the seeded pin untouched at revision 1 (the fixture serves 3)", pins)
		}
		if got := f.configHits(); got != 1 {
			t.Fatalf("GET /v1/sync/config called %d time(s), want exactly 1: the capability is what the refusal reads", got)
		}
		// Ordering, not politeness: the refusal must precede the artifact
		// fetch, so a mutant that fetches first and checks after cannot pass
		// by returning the same error later.
		if q := f.artifactsQueriesSeen(); len(q) != 0 {
			t.Fatalf("GET /v1/sync/artifacts called %d time(s) before refusing, want 0 (raw %v)", len(q), q)
		}
	})

	t.Run("config fetch fails", func(t *testing.T) {
		f := newFakeAPI(t, pinnableArtifact)
		f.pinning = true // never read: the fetch below never returns a body
		f.configStatus = http.StatusInternalServerError
		home := syncFixture(t, f)
		pinsPath := filepath.Join(home, ".config", "orbeat", "pins.json")

		if err := syncclient.SetPin(pinsPath, syncclient.Pin{ArtifactID: idPin, Type: "skill", Name: "pinme", Revision: 1}); err != nil {
			t.Fatalf("seed existing pin: %v", err)
		}

		if err := dispatch(context.Background(), syncclient.Config{APIBaseURL: f.srv.URL}, []string{"pin", "skill/pinme"}); err == nil {
			t.Fatal("pin succeeded although GET /v1/sync/config returned 500, want a refusal")
		}
		pins, lerr := syncclient.LoadPins(pinsPath)
		if lerr != nil {
			t.Fatalf("load pins: %v", lerr)
		}
		if len(pins) != 1 || pins[0].Revision != 1 {
			t.Fatalf("pins = %+v, want the seeded pin untouched at revision 1", pins)
		}
	})
}
