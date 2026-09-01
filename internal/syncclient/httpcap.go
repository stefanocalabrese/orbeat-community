package syncclient

import (
	"encoding/json"
	"fmt"
	"io"
)

// B26: every response body this client decodes comes from a server it does
// not fully control — ORBEAT_API_URL and ORBEAT_OIDC_DISCOVERY_URL are client
// config, so a misconfigured or hostile endpoint at either one decides what
// gets read here. Before decodeJSONCapped existed, every call site was a bare
// json.NewDecoder(resp.Body).Decode with no upper bound at all: a 5 MiB
// `content` field (80x the server's own 64 KiB per-artifact cap) was written
// verbatim to ~/.claude/skills/<name>/SKILL.md. doctor.go's readCapped
// already applies this exact defensive style to local file reads (citing the
// v1.18.0 audit's precedent — auth.maxDiscoveryBodyBytes,
// govern.llmMaxRespBytes, api.maxRequestBodyBytes, all "1 << 20" — for the
// server side of this codebase); this file is the client-side counterpart.
//
// Two tiers, not one, because one bound cannot be both tight and safe for
// every response this client decodes:
//
//   - maxJSONBodyBytes (1 MiB) covers every FIXED-SHAPE response: OIDC
//     discovery documents, the sync config document, device-authorization and
//     token-endpoint responses, and the deployment-report acknowledgement.
//     None of these have an unbounded field, and even a large, comprehensive
//     Keycloak discovery document is realistically a few KB — 1 MiB is
//     already generous by two-plus orders of magnitude, and matches the
//     server-side convention exactly (auth.maxDiscoveryBodyBytes protects
//     the identical document type, just fetched by a different service).
//   - maxArtifactsBodyBytes (16 MiB) covers GET /v1/sync/artifacts alone,
//     which is deliberately unbounded server-side (unlike the paginated
//     admin list endpoints — a partial page here would read as a
//     de-entitlement and delete the developer's files, so pagination was
//     ruled out) and can legitimately carry many entitled artifacts. The
//     server caps each one at 64 KiB content + 16 KiB seed (~80 KiB with
//     JSON overhead); 16 MiB gives headroom for roughly 200 max-sized
//     artifacts served to a single caller in one response — an order of
//     magnitude beyond any deployment this codebase's own docs describe —
//     while still being a bounded, defensive ceiling rather than "unlimited".
const (
	maxJSONBodyBytes      = 1 << 20  // 1 MiB
	maxArtifactsBodyBytes = 16 << 20 // 16 MiB
)

// decodeJSONCapped reads body through a max-byte limit before decoding JSON
// into v, mirroring doctor.go's readCapped: the limit reader is capped one
// byte past max so a response that is EXACTLY max bytes is not mistaken for
// one that overflowed it (the same off-by-one guard readCapped uses), and an
// oversized body is reported as exactly that — "response body exceeds N
// bytes" — rather than surfacing as a confusing mid-parse JSON error from a
// silently truncated read.
func decodeJSONCapped(body io.Reader, max int64, v any) error {
	data, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if int64(len(data)) > max {
		return fmt.Errorf("response body exceeds %d bytes", max)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
