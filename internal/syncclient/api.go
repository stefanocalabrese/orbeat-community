package syncclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Artifact is one entitled artifact returned by GET /v1/sync/artifacts. Content
// is the final, ready-to-write file body (memory frontmatter already injected).
// MemoryScope/MemorySeed are set only for user/project-scope subagents that
// carry a governed seed (spec §6).
//
// ID and Revision mirror the server DTO's two unconditional identity fields:
// the artifact's uuid and the revision_num its content came from. An older
// server that does not send them decodes as "" and 0, which is why nothing
// here negotiates for them.
type Artifact struct {
	ID          string `json:"id"`                 // artifact uuid: stable across renames
	Revision    int    `json:"revision,omitempty"` // revision_num of the served snapshot; 0 = not told
	Type        string `json:"type"`
	Name        string `json:"name"`
	Content     string `json:"content"`
	MemoryScope string `json:"memoryScope,omitempty"` // seed target-path selection (user|project)
	MemorySeed  string `json:"memorySeed,omitempty"`  // governed ORBEAT-SEED block body
	// TargetTags is a rule's approved project targeting (migration 00024).
	// EMPTY MEANS EVERY REGISTERED PROJECT, which is both the pre-targeting
	// behaviour and what an older server sends by not sending the field at
	// all, so no version negotiation is needed in either direction.
	TargetTags []string `json:"targetTags,omitempty"`
	// RuleScope is "global" on a rule belonging in the user-level instruction
	// files rather than in each registered project. Absent means project scope,
	// so a server that predates scoping describes every rule correctly by
	// saying nothing.
	RuleScope string `json:"ruleScope,omitempty"`

	// The pinning half, mirroring syncArtifactDTO's own three conditional
	// fields (internal/api/sync.go): present only when the server supports
	// pinning, omitted wholesale otherwise. OldestServable/Latest are the
	// window 'orbeat-sync pin --revision N' validates against before writing
	// a pin; PinOverride is "" when a held pin was served exactly (or there
	// was none), and "floor"/"pruned"/"ahead" otherwise: runSync turns a
	// non-empty value into a warning naming the pin, the requested revision,
	// the served revision and this reason (cmd/orbeat-sync/outcome.go's pinOutcome).
	// A server that omits these decodes all three as their zero values,
	// which is indistinguishable from "no override" here, so the caller must
	// consult SyncConfig.Pinning first, exactly as it must for
	// DeploymentRegistry, rather than infer support from these being zero.
	OldestServable int    `json:"oldestServable,omitempty"`
	Latest         int    `json:"latest,omitempty"`
	PinOverride    string `json:"pinOverride,omitempty"`
}

// authHint annotates a 401/403 — the server rejected the presented token — with
// the one command that fixes it. Other statuses (5xx, 404, …) are not credential
// problems, so they get no hint: sending the user to re-login would mislead.
func authHint(status int) string {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return " (token rejected — run 'orbeat-sync login')"
	}
	return ""
}

// FetchArtifacts calls GET {baseURL}/v1/sync/artifacts with the bearer token
// and returns the caller's entitled artifacts. pins is appended as repeated
// ?pin=<artifactId>:<revision> query parameters (internal/api/sync_pins.go's
// parsePins); nil or empty sends none, the unpinned request every release
// before this one made.
//
// THIS FUNCTION NEVER CONSULTS SyncConfig.Pinning ITSELF. Deciding whether to
// pass a non-nil pins here is the caller's job: sending ?pin= to a server
// that has not affirmatively advertised support for it is exactly the
// silently-served-latest failure the capability negotiation exists to prevent.
//
// BOTH callers in cmd/orbeat-sync fetch /v1/sync/config before reaching this
// function, and both are named here rather than summarised as "the caller",
// because naming only one is how the second came to be missed: syncOnce
// withholds every pin and warns per held pin, runPinSet refuses to write a pin
// at all (a warning there precedes a pins.json write, so it would record an
// unchosen revision as the developer's own intent). A third caller inherits
// the same obligation and this function cannot enforce it for them.
func FetchArtifacts(ctx context.Context, hc *http.Client, baseURL, accessToken string, pins []Pin) ([]Artifact, error) {
	u := strings.TrimRight(baseURL, "/") + "/v1/sync/artifacts"
	if len(pins) > 0 {
		q := url.Values{}
		for _, p := range pins {
			q.Add("pin", fmt.Sprintf("%s:%d", p.ArtifactID, p.Revision))
		}
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch artifacts: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch artifacts: status %d%s", resp.StatusCode, authHint(resp.StatusCode))
	}
	var body struct {
		Artifacts []Artifact `json:"artifacts"`
	}
	if err := decodeJSONCapped(resp.Body, maxArtifactsBodyBytes, &body); err != nil {
		return nil, fmt.Errorf("fetch artifacts: %w", err)
	}
	return body.Artifacts, nil
}

// SyncConfig is the whole body of GET /v1/sync/config.
//
// DeploymentRegistry is what lets a new client face an old server with no
// negotiation step: a server that predates the registry returns a body with no
// such key, Go decodes the absent bool as false, and the client does not
// report. It is also the ONLY thing the client consults before reporting.
// Inferring it from anything else (an id being present on an artifact, the
// client's own version) would make the decision depend on a fact that happens
// to correlate today.
type SyncConfig struct {
	GatewayURL         string `json:"gateway_url"`
	DeploymentRegistry bool   `json:"deploymentRegistry"`
	// Pinning is the SAME mechanism as DeploymentRegistry carrying more
	// weight (internal/api/sync.go's handleSyncConfig comment): GET
	// /v1/sync/artifacts read no query parameter before this field existed,
	// and net/http rejects no unknown one, so a client sending ?pin= to a
	// server that predates pinning would be silently served the LATEST
	// revision with no error at all: a developer would believe she was
	// held at revision 3 while her machine took revision 9. An old server's
	// body has no pinning key, Go decodes the absence as false, and runSync
	// warns per held pin and syncs latest deliberately instead of
	// accidentally.
	Pinning bool `json:"pinning"`
}

// getSyncConfig fetches and decodes the config document, returning UNPREFIXED
// errors so each exported caller can name its own operation. One decode path
// for one endpoint: two fetchers decoding two disjoint subsets of the same
// document is the hand-copied-projection defect this repo keeps paying for,
// where a field added to the body reaches one caller and not the other.
func getSyncConfig(ctx context.Context, hc *http.Client, baseURL, accessToken string) (SyncConfig, error) {
	url := strings.TrimRight(baseURL, "/") + "/v1/sync/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return SyncConfig{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := hc.Do(req)
	if err != nil {
		return SyncConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SyncConfig{}, fmt.Errorf("status %d%s", resp.StatusCode, authHint(resp.StatusCode))
	}
	var body SyncConfig
	if err := decodeJSONCapped(resp.Body, maxJSONBodyBytes, &body); err != nil {
		return SyncConfig{}, err
	}
	return body, nil
}

// FetchSyncConfig calls GET {baseURL}/v1/sync/config and returns the whole
// document. `sync` calls it to learn whether this server records deployment
// reports; it deliberately does NOT require a gateway URL, which is `connect`'s
// concern alone.
func FetchSyncConfig(ctx context.Context, hc *http.Client, baseURL, accessToken string) (SyncConfig, error) {
	cfg, err := getSyncConfig(ctx, hc, baseURL, accessToken)
	if err != nil {
		return SyncConfig{}, fmt.Errorf("fetch sync config: %w", err)
	}
	return cfg, nil
}

// FetchGatewayURL calls GET {baseURL}/v1/sync/config and returns the gateway URL
// orbeat-sync writes into each tool's MCP config. An empty gateway_url is an
// error HERE and not in getSyncConfig: it is unusable for `connect` and
// irrelevant to every other reader of the same document.
func FetchGatewayURL(ctx context.Context, hc *http.Client, baseURL, accessToken string) (string, error) {
	cfg, err := getSyncConfig(ctx, hc, baseURL, accessToken)
	if err != nil {
		return "", fmt.Errorf("fetch gateway url: %w", err)
	}
	if cfg.GatewayURL == "" {
		return "", fmt.Errorf("fetch gateway url: server returned an empty gateway_url")
	}
	return cfg.GatewayURL, nil
}

// deploymentReportBody is the POST /v1/sync/deployments request.
//
// THERE IS NO USER FIELD, AND THE SERVER ENFORCES THAT. Its decoder sets
// DisallowUnknownFields, so a body carrying any key not named here is a 400
// rather than a silently ignored extra: the reporter is the token's subject,
// never anything on the wire. Adding a field here to "say who we are" would be
// the whole vulnerability, and it would fail loudly on the first request.
type deploymentReportBody struct {
	InstallID string                 `json:"installId"`
	Artifacts []deploymentReportItem `json:"artifacts"`
}

// deploymentReportItem is one artifact this machine applied, at the revision
// the server served it as. Both values come from Artifact's two unconditional
// identity fields by way of AppliedArtifact; nothing here is computed locally.
//
// Pinned is spec sec 9.4's whole payload: "this install applied Revision
// because a local pin named it". `json:"pinned,omitempty"` so an ordinary,
// unpinned entry (the common case) never puts the key on the wire at all,
// which is one of the two independent defences against the skew trap below,
// not the whole defence: ReportDeployments only ever sets this true when its
// caller (cmd/sync's reportDeployments, by way of reportedPinned) has already
// confirmed GET /v1/sync/config advertised pinning: true THIS RUN. Skipping
// that confirmation and relying on omitempty alone would still send `pinned:
// true` to a server whose own deploymentReportEntry has no such field, and
// decodeJSON's DisallowUnknownFields (internal/api/admin_servers.go) turns
// that into a 400 on the WHOLE report, not a value quietly dropped; a failed
// report is a Warning at exit 0 (reportDeployments' own doc comment), so
// that failure would be silent.
type deploymentReportItem struct {
	ArtifactID string `json:"artifactId"`
	Revision   int    `json:"revision"`
	Pinned     bool   `json:"pinned,omitempty"`
}

// DeploymentReport is what the server recorded for one report: rows written,
// and entries it declined because the caller is no longer entitled to them.
// The client cannot compute Dropped itself, which is why the endpoint answers
// 200 with counts rather than 204.
type DeploymentReport struct {
	Recorded int `json:"recorded"`
	Dropped  int `json:"dropped"`
}

// ReportDeployments posts what this install applied, REPLACING everything this
// install previously reported. An empty applied slice is a legitimate report
// that clears the install's rows, which is how a de-entitlement becomes
// visible; the caller is responsible for never sending one built from a run
// that did not inspect the disk (see cmd/sync's reportDeployments).
//
// The artifacts array is always emitted, never null: the server treats the two
// identically and says so, but a wire form that varies with the length of a
// slice is one more thing a reader has to know is harmless.
//
// pinned is looked up by ArtifactID; an id absent from it (or a nil map)
// reports Pinned: false, which deploymentReportItem's omitempty tag then
// keeps off the wire entirely. THIS FUNCTION DOES NOT DECIDE WHICH ARTIFACTS
// COUNT AS PINNED, deliberately: that decision needs GET /v1/sync/config's
// pinning flag, which this function has no way to know was even checked this
// run, let alone what it said. Decorating by artifact id here (rather than
// pushing Pinned down into AppliedArtifact and the three reconcilers) is safe
// specifically because the key is the artifact's own uuid: the registry
// spec's objection to reconstructing an applied set from PATHS
// (internal/syncclient/reconcile.go, ReconcileResult.Applied's own doc
// comment) does not transfer to an id nothing here maps back to a path.
func ReportDeployments(ctx context.Context, hc *http.Client, baseURL, accessToken, installID string, applied []AppliedArtifact, pinned map[string]bool) (DeploymentReport, error) {
	items := make([]deploymentReportItem, 0, len(applied))
	for _, a := range applied {
		items = append(items, deploymentReportItem{ArtifactID: a.ArtifactID, Revision: a.Revision, Pinned: pinned[a.ArtifactID]})
	}
	payload, err := json.Marshal(deploymentReportBody{InstallID: installID, Artifacts: items})
	if err != nil {
		return DeploymentReport{}, fmt.Errorf("report deployments: marshal: %w", err)
	}
	url := strings.TrimRight(baseURL, "/") + "/v1/sync/deployments"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return DeploymentReport{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return DeploymentReport{}, fmt.Errorf("report deployments: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DeploymentReport{}, fmt.Errorf("report deployments: status %d%s", resp.StatusCode, authHint(resp.StatusCode))
	}
	var out DeploymentReport
	if err := decodeJSONCapped(resp.Body, maxJSONBodyBytes, &out); err != nil {
		return DeploymentReport{}, fmt.Errorf("report deployments: %w", err)
	}
	return out, nil
}
