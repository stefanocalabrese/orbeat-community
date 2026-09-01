package syncclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

func TestFetchArtifacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sync/artifacts" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer AT" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifacts":[
			{"type":"skill","name":"fmt","content":"---\nname: fmt\n---\nx"},
			{"type":"subagent","name":"rev","content":"---\nname: rev\nmemory: project\n---\ny"}
		]}`))
	}))
	defer srv.Close()

	arts, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "AT", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(arts) != 2 || arts[0].Name != "fmt" || arts[1].Type != "subagent" {
		t.Fatalf("bad artifacts: %+v", arts)
	}
}

func TestFetchArtifactsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "bad", nil); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestFetchArtifactsDecodesSeedFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifacts":[
			{"type":"subagent","name":"seeded","content":"c","memoryScope":"project","memorySeed":"seed body"},
			{"type":"skill","name":"plain","content":"c"}]}`))
	}))
	defer srv.Close()

	arts, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "tok", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("want 2 artifacts, got %d", len(arts))
	}
	if arts[0].MemoryScope != "project" || arts[0].MemorySeed != "seed body" {
		t.Fatalf("seed fields not decoded: %+v", arts[0])
	}
	if arts[1].MemoryScope != "" || arts[1].MemorySeed != "" {
		t.Fatalf("absent fields must decode to empty: %+v", arts[1])
	}
}

// S8c: a 401/403 means the server rejected the token — the error must tell the
// user the one command that fixes it. Any other non-200 must NOT carry the hint
// (a 500 is not a credential problem; sending the user to re-login would be a lie).
func TestFetchArtifactsTokenRejectedHint(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			_, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "bad", nil)
			if err == nil || !strings.Contains(err.Error(), "token rejected — run 'orbeat-sync login'") {
				t.Fatalf("status %d error must carry the re-login hint, got: %v", code, err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "tok", nil)
	if err == nil || strings.Contains(err.Error(), "token rejected") {
		t.Fatalf("a 500 must not carry the re-login hint, got: %v", err)
	}
}

// FetchArtifacts appends one repeated ?pin=<artifactId>:<revision> parameter
// per held pin, in order, and sends no query string at all when there are
// none, the unpinned request every release before this one made.
func TestFetchArtifactsSendsPinParams(t *testing.T) {
	var gotQuery map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifacts":[]}`))
	}))
	defer srv.Close()

	pins := []Pin{
		{ArtifactID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Revision: 2},
		{ArtifactID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Revision: 5},
	}
	if _, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "AT", pins); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got := gotQuery["pin"]
	want := []string{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa:2", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb:5"}
	if len(got) != len(want) {
		t.Fatalf("pin params = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pin params = %v, want %v", got, want)
		}
	}
}

func TestFetchArtifactsSendsNoQueryWhenNoPins(t *testing.T) {
	var gotRawQuery string
	var sawQuery bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		sawQuery = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifacts":[]}`))
	}))
	defer srv.Close()
	if _, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "AT", nil); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !sawQuery {
		t.Fatal("the fake server never saw the request")
	}
	if gotRawQuery != "" {
		t.Fatalf("raw query = %q, want empty when no pins are held", gotRawQuery)
	}
}

func TestFetchGatewayURLTokenRejectedHint(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			_, err := FetchGatewayURL(context.Background(), srv.Client(), srv.URL, "bad")
			if err == nil || !strings.Contains(err.Error(), "token rejected — run 'orbeat-sync login'") {
				t.Fatalf("status %d error must carry the re-login hint, got: %v", code, err)
			}
		})
	}
}

func TestFetchGatewayURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sync/config" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer AT" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gateway_url":"https://gw"}`))
	}))
	defer srv.Close()

	gw, err := FetchGatewayURL(context.Background(), srv.Client(), srv.URL, "AT")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gw != "https://gw" {
		t.Fatalf("gateway url = %q, want https://gw", gw)
	}
}

func TestFetchGatewayURLNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := FetchGatewayURL(context.Background(), srv.Client(), srv.URL, "bad"); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestFetchGatewayURLEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gateway_url":""}`))
	}))
	defer srv.Close()
	if _, err := FetchGatewayURL(context.Background(), srv.Client(), srv.URL, "tok"); err == nil {
		t.Fatal("expected error on empty gateway_url")
	}
}

// An older server returns a config body with no deploymentRegistry key at all.
// Go decodes the absent bool as false and the client does not report: the
// degradation is correct with zero cooperation from the old server, which is
// the whole reason there is no negotiation step.
func TestFetchSyncConfigDecodesTheRegistryFlag(t *testing.T) {
	cases := map[string]struct {
		body string
		want bool
	}{
		"advertised": {`{"gateway_url":"https://gw","deploymentRegistry":true}`, true},
		"declined":   {`{"gateway_url":"https://gw","deploymentRegistry":false}`, false},
		"old server": {`{"gateway_url":"https://gw"}`, false},
		"no gateway": {`{"deploymentRegistry":true}`, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/sync/config" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			cfg, err := FetchSyncConfig(context.Background(), srv.Client(), srv.URL, "AT")
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if cfg.DeploymentRegistry != c.want {
				t.Fatalf("DeploymentRegistry = %v, want %v (body %s)", cfg.DeploymentRegistry, c.want, c.body)
			}
		})
	}
}

// FetchSyncConfig must not require a gateway URL: `connect` needs one, `sync`
// does not, and a shared fetcher that failed on the empty case would make the
// registry flag unreadable on a server with no gateway configured.
func TestFetchSyncConfigToleratesAnEmptyGatewayURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gateway_url":"","deploymentRegistry":true}`))
	}))
	defer srv.Close()
	cfg, err := FetchSyncConfig(context.Background(), srv.Client(), srv.URL, "AT")
	if err != nil {
		t.Fatalf("an empty gateway_url must not fail the config read: %v", err)
	}
	if !cfg.DeploymentRegistry {
		t.Fatal("DeploymentRegistry = false, want true")
	}
}

// The report's wire shape, asserted key by key. The server decodes with
// DisallowUnknownFields, so a body carrying anything not named here is a 400
// rather than an ignored extra: this gate is what keeps a well-meant addition
// (a hostname, a username, a "userId" naming who we think we are) from
// shipping as a client that cannot report at all.
func TestReportDeploymentsSendsExactlyInstallIDAndArtifacts(t *testing.T) {
	var (
		gotMethod, gotPath, gotAuth, gotType string
		gotBody                              []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recorded":2,"dropped":1}`))
	}))
	defer srv.Close()

	res, err := ReportDeployments(context.Background(), srv.Client(), srv.URL, "AT",
		"11111111-1111-4111-8111-111111111111",
		[]AppliedArtifact{{ArtifactID: "a-id", Revision: 4}, {ArtifactID: "b-id", Revision: 7}}, nil)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/sync/deployments" {
		t.Fatalf("sent %s %s, want POST /v1/sync/deployments", gotMethod, gotPath)
	}
	if gotAuth != "Bearer AT" {
		t.Fatalf("Authorization = %q, want the bearer token", gotAuth)
	}
	if gotType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotType)
	}
	if res.Recorded != 2 || res.Dropped != 1 {
		t.Fatalf("decoded %+v, want {2 1}", res)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &top); err != nil {
		t.Fatalf("body does not decode: %v (%s)", err, gotBody)
	}
	if len(top) != 2 || top["installId"] == nil || top["artifacts"] == nil {
		t.Fatalf("body keys = %v, want exactly installId and artifacts (%s)", keysOf(top), gotBody)
	}
	if string(top["installId"]) != `"11111111-1111-4111-8111-111111111111"` {
		t.Fatalf("installId = %s", top["installId"])
	}
	var items []map[string]any
	if err := json.Unmarshal(top["artifacts"], &items); err != nil {
		t.Fatalf("artifacts does not decode: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("sent %d artifact(s), want 2 (%s)", len(items), gotBody)
	}
	for i, want := range []struct {
		id  string
		rev float64
	}{{"a-id", 4}, {"b-id", 7}} {
		if len(items[i]) != 2 || items[i]["artifactId"] != want.id || items[i]["revision"] != want.rev {
			t.Fatalf("artifacts[%d] = %v, want exactly {artifactId:%s revision:%v}", i, items[i], want.id, want.rev)
		}
	}
}

// TestReportDeploymentsSendsPinnedOnlyWhenTrue is the DIRECT wire-level
// observation for the skew trap's two defences (deploymentReportItem's
// `json:"pinned,omitempty"` tag plus cmd/sync's gate on scfg.Pinning, whose
// own doc comments name each other as the two halves): "pinned" reaches the
// server ON THE WIRE for an artifact the pinned map marks true, and is
// ABSENT, not merely false, for one it does not.
//
// A gate asserting only the decoded DeploymentReport (recorded/dropped counts)
// cannot see this: both artifacts below record identically either way. Only
// reading the raw body tells "the key is absent" apart from "the key is
// present and false", and it is exactly that distinction a server with the
// registry but not pinning cannot survive (DisallowUnknownFields 400s the
// whole report the moment the key exists at all, whatever its value).
func TestReportDeploymentsSendsPinnedOnlyWhenTrue(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recorded":2,"dropped":0}`))
	}))
	defer srv.Close()

	pinned := map[string]bool{"a-id": true} // b-id deliberately absent, not set false
	if _, err := ReportDeployments(context.Background(), srv.Client(), srv.URL, "AT",
		"11111111-1111-4111-8111-111111111111",
		[]AppliedArtifact{{ArtifactID: "a-id", Revision: 4}, {ArtifactID: "b-id", Revision: 7}}, pinned); err != nil {
		t.Fatalf("report: %v", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &top); err != nil {
		t.Fatalf("body does not decode: %v (%s)", err, gotBody)
	}
	var items []map[string]any
	if err := json.Unmarshal(top["artifacts"], &items); err != nil {
		t.Fatalf("artifacts does not decode: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("sent %d artifact(s), want 2 (%s)", len(items), gotBody)
	}
	if v, ok := items[0]["pinned"]; !ok || v != true {
		t.Fatalf("artifacts[0] (a-id, pinned) = %v, want a literal \"pinned\":true on the wire (%s)", items[0], gotBody)
	}
	if _, ok := items[1]["pinned"]; ok {
		t.Fatalf("artifacts[1] (b-id, not pinned) = %v, want NO \"pinned\" key at all on the wire, "+
			"not a false one (%s)", items[1], gotBody)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// An empty applied set is a legitimate report that clears this install's rows,
// and it must reach the server as [] rather than null. Both are accepted, but
// a wire form that varies with a slice's length is one more thing a reader has
// to know is harmless.
func TestReportDeploymentsSendsAnEmptyArrayNotNull(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recorded":0,"dropped":0}`))
	}))
	defer srv.Close()

	if _, err := ReportDeployments(context.Background(), srv.Client(), srv.URL, "AT",
		"11111111-1111-4111-8111-111111111111", nil, nil); err != nil {
		t.Fatalf("report: %v", err)
	}
	if !strings.Contains(string(body), `"artifacts":[]`) {
		t.Fatalf("empty report body = %s, want an empty artifacts array", body)
	}
}

func TestReportDeploymentsTokenRejectedHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, err := ReportDeployments(context.Background(), srv.Client(), srv.URL, "bad",
		"11111111-1111-4111-8111-111111111111", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "token rejected") {
		t.Fatalf("a 401 must carry the re-login hint, got: %v", err)
	}
}

func TestReportDeploymentsNon200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := ReportDeployments(context.Background(), srv.Client(), srv.URL, "AT",
		"11111111-1111-4111-8111-111111111111", []AppliedArtifact{{ArtifactID: "x", Revision: 2}}, nil); err == nil {
		t.Fatal("expected an error on 500")
	}
}

// --- B26: every network read this client decodes must be capped ---

// TestFetchArtifactsRefusesAnOversizedResponseBody reproduces the audit's own
// measurement: a huge `content` field written verbatim, because
// json.NewDecoder(resp.Body).Decode has no upper bound at all. ORBEAT_API_URL
// is client config, so a misconfigured or hostile endpoint controls what
// this decodes; before this fix, a 5 MiB content field (80x the server's own
// 64 KiB per-artifact cap) landed on disk unchanged.
func TestFetchArtifactsRefusesAnOversizedResponseBody(t *testing.T) {
	huge := strings.Repeat("A", int(maxArtifactsBodyBytes)+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifacts":[{"type":"skill","name":"huge","content":"`))
		_, _ = w.Write([]byte(huge))
		_, _ = w.Write([]byte(`"}]}`))
	}))
	defer srv.Close()

	arts, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "AT", nil)
	if err == nil {
		t.Fatalf("an oversized response body must be refused, got %d artifacts with no error", len(arts))
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("the error must name the size limit, got: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("a refused response must decode nothing, got %+v", arts)
	}
}

// Non-vacuity: a legitimate, large-but-under-cap response (many artifacts,
// comfortably under maxArtifactsBodyBytes) must still decode successfully —
// the cap must not break the real thing it exists to protect.
func TestFetchArtifactsAcceptsALegitimateLargeResponse(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"artifacts":[`)
	const n = 2000
	body := strings.Repeat("x", 2000) // ~2KB content per artifact, ~4MB total: well under the cap, well over a trivial response
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"type":"skill","name":"a%d","content":%q}`, i, body)
	}
	b.WriteString(`]}`)
	payload := b.String()
	if int64(len(payload)) >= maxArtifactsBodyBytes {
		t.Fatalf("test fixture itself exceeds the cap (%d >= %d) — not a legitimate-large fixture", len(payload), maxArtifactsBodyBytes)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	arts, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "AT", nil)
	if err != nil {
		t.Fatalf("a legitimate large-but-under-cap response must decode, got: %v", err)
	}
	if len(arts) != n {
		t.Fatalf("got %d artifacts, want %d", len(arts), n)
	}
}

func TestFetchSyncConfigRefusesAnOversizedResponseBody(t *testing.T) {
	huge := strings.Repeat("A", int(maxJSONBodyBytes)+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gateway_url":"`))
		_, _ = w.Write([]byte(huge))
		_, _ = w.Write([]byte(`"}`))
	}))
	defer srv.Close()
	if _, err := FetchSyncConfig(context.Background(), srv.Client(), srv.URL, "AT"); err == nil {
		t.Fatal("an oversized /v1/sync/config body must be refused")
	}
}

// The padding rides in an UNKNOWN field, not in "recorded" itself: a huge
// digit run stuffed into an int field fails to parse from integer overflow
// alone, on ANY decoder, capped or not — that would make this test pass
// vacuously regardless of whether the cap is wired in at all. An unknown
// field is silently ignored by json.Unmarshal (this client's decode does not
// set DisallowUnknownFields, unlike the server's own request decoding), so on
// the OLD uncapped code this body decodes just fine; only the cap refuses it.
func TestReportDeploymentsRefusesAnOversizedResponseBody(t *testing.T) {
	huge := strings.Repeat("A", int(maxJSONBodyBytes)+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recorded":2,"dropped":0,"padding":"`))
		_, _ = w.Write([]byte(huge))
		_, _ = w.Write([]byte(`"}`))
	}))
	defer srv.Close()
	if _, err := ReportDeployments(context.Background(), srv.Client(), srv.URL, "AT",
		"11111111-1111-4111-8111-111111111111", nil, nil); err == nil {
		t.Fatal("an oversized deployment-report response body must be refused")
	}
}
