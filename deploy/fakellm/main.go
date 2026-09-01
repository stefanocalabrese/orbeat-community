// Command fakellm is a stand-in LLM endpoint for orbeat's dev compose stack.
//
// WHY IT EXISTS. The advisory LLM content scanner
// (internal/govern/llm_scanner.ee.go) is off unless ORBEAT_SCAN_LLM_ENDPOINT
// names a model, and no deployment of orbeat has ever named one. That layer
// has therefore never run outside Go unit tests, where the llmClient is a stub
// and no HTTP request is ever made. This service is the endpoint the dev stack
// points at, so `make smoke` and the Playwright e2e suite drive the real path:
// the secrets ref resolves to a key, openAIClient builds a request, the request
// crosses a network, the response is parsed, findings are clamped to warn/info
// and prefixed with "llm-", and handleSubmitArtifact acts on them.
//
// WHAT IT BUYS, AND WHAT IT DOES NOT. It buys coverage of that path. It buys
// NO precision data about the real scanner. Every finding below is canned;
// nothing here reads content and forms a judgement about it. "Should an LLM
// finding be allowed to block a submit" is a question about a real model's
// false-positive rate on real artifacts, and this service can neither answer
// it nor contribute evidence toward it. Do not cite a green run here as
// support for that decision.
//
// THE WIRE SHAPE IS NOT INVENTED. It is exactly what openAIClient.Complete in
// internal/govern/llm_client.ee.go already sends and reads: a POST to
// <endpoint>/v1/chat/completions carrying an "authorization: Bearer <key>"
// header and a JSON body with "model" and a "messages" array of
// {role, content} objects, answered with a JSON body whose
// choices[0].message.content is a string. The scanner then pulls the substring
// from that content's first "{" to its last "}" and decodes it as
// {"findings":[{rule,message,severity}]} (parseLLMFindings, same package).
//
// Only the OpenAI-compatible route is served. The client also speaks the
// Anthropic Messages shape (POST /v1/messages), but the compose file sets
// ORBEAT_SCAN_LLM_PROVIDER=openai, so an Anthropic route here would be code no
// configuration reaches: a second wire shape with no gate on it is the kind of
// thing that rots silently. Add it in the same change that adds a stack which
// selects it.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// listenAddr is fixed rather than read from an env var. This service has
	// exactly one consumer (the api container, by compose service name) and
	// one healthcheck (the self-probe below), and both would have to agree
	// with any override, so an override is two ways to be wrong in exchange
	// for nothing.
	listenAddr = ":9100"

	// maxReqBytes bounds the request body. An artifact's content is capped at
	// 64KiB and its seed at 16KiB (govern.MaxContentBytes / MaxSeedBytes), so
	// 4MiB is far above anything the real client can send while still
	// refusing an unbounded read.
	maxReqBytes = 4 << 20
)

// sentinel is the ONLY string this service flags.
//
// THIS CONSTANT DECIDES THE WHOLE DESIGN. Once the dev stack points the
// scanner here, every artifact `make smoke` creates and every artifact the
// Playwright suite creates is scanned by this service. A fake that flagged
// anything it found interesting would put a finding on all of them, and a
// finding is not cosmetic: internal/api/admin_artifact_review.ee.go gates
// trusted-author auto-approval on len(findings) == 0, so artifacts other specs
// expect approved would sit at pending, and the portal would grow an
// "acknowledge findings" prompt on rows whose specs assert there is none.
// The result would be a wall of failures about something other than this
// feature. RETURNING AN EMPTY FINDINGS LIST FOR EVERYTHING ELSE IS WHAT KEEPS
// THE REST OF THE SUITE MEANINGFUL, and it is not a shortcut to be tidied
// away later: the flagging behaviour a test wants must be requested by that
// test's own content, never volunteered by this service.
//
// The value is chosen so it can collide with nothing:
//
//   - Not real artifact content. It is a fixed uppercase token with no
//     natural-language meaning, and it appeared nowhere in the repository
//     when it was introduced.
//   - Not the deterministic scanner's rules (internal/govern/scanner.go). It
//     is not an AWS/Google/GitHub/Slack key shape, not a PEM private-key
//     header, and not the "curl ... | bash" remote-exec pattern. It carries
//     the ORBEAT- prefix but is deliberately NOT the managed-block marker
//     shape "<!-- ORBEAT-SEED:BEGIN" / "<!-- ORBEAT-RULES:BEGIN" that
//     reservedMarkerRe blocks, so content carrying it reaches this service
//     rather than being rejected before the scan.
//
// Reusing an existing sentinel would have been worse than inventing one: a
// test whose content trips both scanners cannot tell which of them fired.
const sentinel = "ORBEAT-FAKE-LLM-FLAG-THIS-ARTIFACT"

// chatRequest is the subset of the request body openAIClient.Complete sends
// that this service reads. max_tokens and response_format are deliberately
// absent: nothing here generates tokens or needs telling to emit JSON.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the OpenAI-compatible response. The client reads only
// choices[0].message.content; the surrounding fields are here so the body is
// recognisably the real shape rather than the narrowest thing that happens to
// parse.
type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// finding mirrors govern.Finding's JSON tags. It is redeclared rather than
// imported because deploy/ services are stack fixtures, not orbeat internals,
// and the shape they must produce is the one on the wire.
type finding struct {
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

type findingsEnvelope struct {
	Findings []finding `json:"findings"`
}

func main() {
	// `-healthcheck` is the container self-probe, matching cmd/api and
	// cmd/portal: the distroless image has no shell or curl, so the compose
	// healthcheck invokes the binary itself.
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthProbe())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handleChatCompletions)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("fakellm: listening on %s, flagging only %s", listenAddr, sentinel)
	log.Fatal(srv.ListenAndServe())
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Requiring a non-empty bearer token is the one assertion this service
	// makes about its caller, and it covers a real segment of the path: the
	// api resolves ORBEAT_SCAN_LLM_KEY_REF through internal/secrets at
	// startup (fail-closed) and openAIClient attaches the result here. The
	// value is not compared against an expected one, because the api cannot
	// start at all with an unresolvable ref, so "some key arrived" is
	// everything the check can establish.
	if strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) == "" {
		writeError(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReqBytes)).Decode(&req); err != nil {
		// Loud on purpose. If the client's request shape ever drifts away
		// from what this decodes, the scanner's fail-open path turns that
		// into an "info" finding on every submit, which stops trusted-author
		// auto-approval and is impossible to miss. A fake that shrugged and
		// answered 200 anyway would hide exactly the drift it is here to
		// catch.
		writeError(w, http.StatusBadRequest, "undecodable request body")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "no messages in request")
		return
	}

	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	scanned := b.String()
	flagged := strings.Contains(scanned, sentinel)

	// Non-nil so an unflagged scan marshals as "findings":[] rather than
	// "findings":null. parseLLMFindings accepts both, but [] is the shape the
	// system prompt asks a real model for.
	findings := []finding{}
	if flagged {
		findings = append(findings, finding{
			// govern's llmScanner prefixes this with "llm-", so the stored
			// rule reads llm-fake-sentinel and is distinguishable from every
			// deterministic rule (secret, reserved-marker, remote-exec, size).
			Rule:     "fake-sentinel",
			Message:  "content carries the dev stack's fake-LLM sentinel; this finding is canned, not a judgement about the content",
			Severity: "warn",
		})
	}

	content, err := json.Marshal(findingsEnvelope{Findings: findings})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode findings")
		return
	}

	// Deliberately never logs the scanned text. This is a dev fixture, but it
	// is handed whole artifact bodies, and the production client goes out of
	// its way to keep content out of every error string it builds; a fixture
	// that dumps it into container logs would undo that on the one stack
	// people actually run.
	log.Printf("fakellm: scan model=%q messages=%d bytes=%d flagged=%t",
		req.Model, len(req.Messages), len(scanned), flagged)

	writeJSON(w, http.StatusOK, chatResponse{
		ID:      "fakellm-scan",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      chatMessage{Role: "assistant", Content: string(content)},
			FinishReason: "stop",
		}},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	log.Printf("fakellm: %d %s", status, msg)
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg}})
}

// healthProbe performs a GET on this service's own /healthz and returns 0 on a
// 200, 1 otherwise.
func healthProbe() int {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1" + listenAddr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: /healthz returned", resp.StatusCode)
		return 1
	}
	return 0
}
