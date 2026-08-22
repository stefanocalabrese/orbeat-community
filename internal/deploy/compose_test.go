// Package deploy holds invariants about the repo's deployment config.
//
// It exists because the observability profile has several ways to be silently
// wrong that no other gate can see: a passthrough attached to the wrong
// service, a floating image tag that bypasses Dependabot, a port that drifts
// off loopback, or a depends_on that makes the DEFAULT stack unstartable.
// `docker compose config -q` validates syntax; it does not encode intent.
package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// devComposePath and prodComposePath are the two compose files this package
// reads. composeServices is parameterized by path (rather than hard-coding
// devComposePath) because a helper that can only read the file it was
// written for is the same defect as a suite that only gates the dev stack —
// see docs/specs/2026-08-19-orbeat-prod-config-gate-design.md §5.
const (
	devComposePath  = "deploy/docker-compose.yml"
	prodComposePath = "deploy/docker-compose.prod.yml"
)

func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

func composeServices(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(repoFile(t, path), &doc); err != nil {
		t.Fatalf("parse compose %s: %v", path, err)
	}
	return doc.Services
}

// TestOTelPassthroughOnBothExportingServices pins the line the whole profile
// depends on. Compose profiles gate SERVICES, not variables on other services,
// so starting the collector does nothing unless api and gateway are told to
// export to it — and a deleted line is invisible to `docker compose config -q`,
// which validates syntax rather than intent.
func TestOTelPassthroughOnBothExportingServices(t *testing.T) {
	svcs := composeServices(t, devComposePath)
	for _, name := range []string{"api", "gateway"} {
		svc, ok := svcs[name]
		if !ok {
			t.Fatalf("service %q missing from the compose file", name)
		}
		env, ok := svc["environment"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no environment map", name)
		}
		got, ok := env["ORBEAT_OTEL_ENDPOINT"]
		if !ok {
			t.Fatalf("%s is missing ORBEAT_OTEL_ENDPOINT — the observability profile cannot export without it", name)
		}
		if s, _ := got.(string); !strings.Contains(s, "${ORBEAT_OTEL_ENDPOINT") {
			t.Errorf("%s ORBEAT_OTEL_ENDPOINT = %v, want the ${ORBEAT_OTEL_ENDPOINT:-} passthrough so unset stays disabled", name, got)
		}
	}
}

// TestArtifactRevisionKeepPassthroughOnAPI pins the line the revision-pruning
// cap depends on (docs/specs/2026-08-19-orbeat-revision-pruning-design.md
// §8). Unlike ORBEAT_OTEL_ENDPOINT, only the api service reads this knob
// (internal/config.ArtifactRevisionKeepN, wired by cmd/api/main.go) — the
// gateway never approves or rolls back an artifact — so the gate checks api
// only. A hard-coded value here would enable pruning for every developer's
// `make up` and for the Playwright e2e job, both of which run this same
// compose file: portal/e2e/approval.spec.ts approves an artifact twice then
// rolls back to revision 1, which a hard-coded cap of 2 would survive with
// zero margin.
func TestArtifactRevisionKeepPassthroughOnAPI(t *testing.T) {
	svc, ok := composeServices(t, devComposePath)["api"]
	if !ok {
		t.Fatal(`service "api" missing from the compose file`)
	}
	env, ok := svc["environment"].(map[string]any)
	if !ok {
		t.Fatal("api has no environment map")
	}
	got, ok := env["ORBEAT_ARTIFACT_REVISION_KEEP"]
	if !ok {
		t.Fatal("api is missing ORBEAT_ARTIFACT_REVISION_KEEP — the revision-pruning cap can never be set")
	}
	if s, _ := got.(string); !strings.Contains(s, "${ORBEAT_ARTIFACT_REVISION_KEEP") {
		t.Errorf("api ORBEAT_ARTIFACT_REVISION_KEEP = %v, want the ${ORBEAT_ARTIFACT_REVISION_KEEP:-} passthrough so unset stays disabled", got)
	}
}

// TestObservabilityServicesAreProfileGated pins that the default stack is
// unaffected. A service that loses its profile starts on every `make up`,
// `make smoke` and e2e run.
func TestObservabilityServicesAreProfileGated(t *testing.T) {
	svcs := composeServices(t, devComposePath)
	for _, name := range []string{"otel-collector", "jaeger"} {
		svc, ok := svcs[name]
		if !ok {
			t.Fatalf("service %q missing", name)
		}
		profs, ok := svc["profiles"].([]any)
		if !ok || len(profs) == 0 {
			t.Fatalf("%s has no profiles — it would start with the default stack", name)
		}
		found := false
		for _, p := range profs {
			if s, _ := p.(string); s == "observability" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s profiles = %v, want observability", name, profs)
		}
	}
}

// TestNoUnprofiledServiceDependsOnAProfiledOne is the one that protects the
// DEFAULT stack. A non-profiled service depending on a profiled one is not a
// runtime degradation: compose rejects the whole project with "depends on
// undefined service", so `make up` fails outright.
func TestNoUnprofiledServiceDependsOnAProfiledOne(t *testing.T) {
	svcs := composeServices(t, devComposePath)
	profiled := map[string]bool{}
	for name, svc := range svcs {
		if p, ok := svc["profiles"].([]any); ok && len(p) > 0 {
			profiled[name] = true
		}
	}
	for name, svc := range svcs {
		if profiled[name] {
			continue // profiled services may depend on each other
		}
		for _, dep := range dependsOn(svc) {
			if profiled[dep] {
				t.Errorf("unprofiled service %q depends on profiled %q — compose rejects the whole project, so the DEFAULT stack would not start", name, dep)
			}
		}
	}
}

// dependsOn normalises compose's two depends_on forms: a list of names, or a
// map of name to condition.
func dependsOn(svc map[string]any) []string {
	switch d := svc["depends_on"].(type) {
	case []any:
		var out []string
		for _, v := range d {
			if s, _ := v.(string); s != "" {
				out = append(out, s)
			}
		}
		return out
	case map[string]any:
		var out []string
		for k := range d {
			out = append(out, k)
		}
		return out
	}
	return nil
}

// imageIsPinned reports whether img (a compose "image:" value) names an
// explicit tag rather than floating (no tag, "latest", or a tag-shaped
// segment that is actually a registry path with no tag at all).
func imageIsPinned(img string) bool {
	tag := img[strings.LastIndex(img, ":")+1:]
	return strings.Contains(img, ":") && tag != "latest" && !strings.Contains(tag, "/")
}

// TestEveryImageIsPinned guards the supply chain. Dependabot's docker-compose
// ecosystem watches this file, so a pinned tag gets bumped and reviewed — a
// floating one silently bypasses that, and the release Trivy gate only scans
// images this repo BUILDS, so a pulled image is scanned by nothing else.
func TestEveryImageIsPinned(t *testing.T) {
	for name, svc := range composeServices(t, devComposePath) {
		img, ok := svc["image"].(string)
		if !ok {
			continue // built from a Dockerfile, not pulled
		}
		if !imageIsPinned(img) {
			t.Errorf("service %q image %q is not pinned to an explicit tag", name, img)
		}
	}
}

// TestPublishedPortsAreLoopbackBound pins the v1.17.0 convention: the audit
// found services on 0.0.0.0, and every published port has been 127.0.0.1-bound
// since. A new service that forgets it exposes the dev stack to the network.
func TestPublishedPortsAreLoopbackBound(t *testing.T) {
	for name, svc := range composeServices(t, devComposePath) {
		ports, ok := svc["ports"].([]any)
		if !ok {
			continue
		}
		for _, p := range ports {
			s, _ := p.(string)
			if !strings.HasPrefix(s, "127.0.0.1:") {
				t.Errorf("service %q publishes %q — every published port must bind 127.0.0.1 (v1.17.0)", name, s)
			}
		}
	}
}

// TestCollectorConfigUsesTheOTLPJaegerExporter pins the trap that kills the
// collector at startup: the `jaeger` exporter was REMOVED, and the obvious
// spelling fails with `unknown type: "jaeger"`. It also checks every pipeline
// references an exporter that is actually defined.
func TestCollectorConfigUsesTheOTLPJaegerExporter(t *testing.T) {
	var doc struct {
		Exporters map[string]any `yaml:"exporters"`
		Service   struct {
			Pipelines map[string]struct {
				Exporters []string `yaml:"exporters"`
			} `yaml:"pipelines"`
		} `yaml:"service"`
	}
	if err := yaml.Unmarshal(repoFile(t, "deploy/otel-collector.yaml"), &doc); err != nil {
		t.Fatalf("parse collector config: %v", err)
	}
	if _, bad := doc.Exporters["jaeger"]; bad {
		t.Error(`exporter "jaeger" does not exist in the Collector any more — the traces pipeline must use "otlp/jaeger"`)
	}
	if _, ok := doc.Exporters["otlp/jaeger"]; !ok {
		t.Error(`missing the "otlp/jaeger" exporter`)
	}
	for name, p := range doc.Service.Pipelines {
		for _, e := range p.Exporters {
			if _, ok := doc.Exporters[e]; !ok {
				t.Errorf("pipeline %q references undefined exporter %q — the collector refuses to start", name, e)
			}
		}
	}
}

// --- deploy/docker-compose.prod.yml -----------------------------------
//
// v1.20.0's own postmortem is a list of prod-only defects that
// `docker compose config -q` and code review both missed — a missing
// `--import-realm`, a missing KC_HOSTNAME_BACKCHANNEL_DYNAMIC, an invalid
// KC_PROXY_TRUSTED_ADDRESSES, and the gateway under a path prefix that
// breaks MCP OAuth discovery — each of them a static property of this one
// file. These gates pin them at `go test` speed, independent of whether
// anyone runs `make smoke-prod`
// (docs/specs/2026-08-19-orbeat-prod-config-gate-design.md).
//
// Deliberately NOT gated: whether the pinned tag is the newest release. The
// file pins 1.24.0 while CHANGELOG.md's newest section is v1.25.0, and
// 1.24.0 is correct today because release.yml has never run for the v1.25.0
// tag — no 1.25.0 image exists to pin. A "matches the newest changelog
// version" gate would fire spuriously and get disabled within a week; tag
// staleness belongs to the release row (spec §3.1).

// TestProdEveryImageIsPinned is TestEveryImageIsPinned's prod counterpart —
// see that test's doc comment for why a floating tag matters.
func TestProdEveryImageIsPinned(t *testing.T) {
	for name, svc := range composeServices(t, prodComposePath) {
		img, ok := svc["image"].(string)
		if !ok {
			continue // built from a Dockerfile, not pulled
		}
		if !imageIsPinned(img) {
			t.Errorf("service %q image %q is not pinned to an explicit tag", name, img)
		}
	}
}

// TestProdOrbeatImagesShareOneTag pins the release invariant: api, gateway
// and portal are built and published together by release.yml on one tag, so
// a partial bump (one service edited, the others forgotten) would leave the
// stack running mismatched versions of the same release.
func TestProdOrbeatImagesShareOneTag(t *testing.T) {
	svcs := composeServices(t, prodComposePath)
	tags := map[string]string{}
	for _, name := range []string{"api", "gateway", "portal"} {
		svc, ok := svcs[name]
		if !ok {
			t.Fatalf("service %q missing from the prod compose file", name)
		}
		img, ok := svc["image"].(string)
		if !ok {
			t.Fatalf("service %q has no image", name)
		}
		tags[name] = img[strings.LastIndex(img, ":")+1:]
	}
	for name, tag := range tags {
		if tag != tags["api"] {
			t.Errorf("service %q pins tag %q, api pins %q — the three orbeat-* images must share one tag or the stack runs mismatched versions of the same release", name, tag, tags["api"])
		}
	}
}

// TestProdOnlyCaddyPublishesPorts is the prod analogue of
// TestPublishedPortsAreLoopbackBound: in this topology Caddy is the sole
// internet-facing service, terminating TLS for every other component, which
// publishes no host port at all. Anything else publishing a port bypasses
// that boundary and becomes directly reachable from the internet.
func TestProdOnlyCaddyPublishesPorts(t *testing.T) {
	svcs := composeServices(t, prodComposePath)
	for name, svc := range svcs {
		ports, ok := svc["ports"].([]any)
		if !ok || len(ports) == 0 {
			continue
		}
		if name != "caddy" {
			t.Errorf("service %q publishes ports %v — only caddy should be internet-facing in the prod topology", name, ports)
			continue
		}
		want := map[string]bool{"80:80": false, "443:443": false}
		for _, p := range ports {
			s, _ := p.(string)
			if _, ok := want[s]; !ok {
				t.Errorf("caddy publishes unexpected port %q, want only 80:80 and 443:443", s)
				continue
			}
			want[s] = true
		}
		for port, seen := range want {
			if !seen {
				t.Errorf("caddy does not publish %q", port)
			}
		}
		if len(ports) != len(want) {
			t.Errorf("caddy publishes %d ports %v, want exactly %d (80 and 443)", len(ports), ports, len(want))
		}
	}
}

// wantPostgresMajor is the postgres major this repo is built, tested and
// documented against. Moving it is a deliberate operator-visible event, never
// a routine image bump: see TestPostgresMajorIsPinnedEverywhere for why, and
// docs/upgrade-guide.md for the procedure that has to ship alongside it.
const wantPostgresMajor = "18"

// TestPostgresMajorIsPinnedEverywhere pins the v1.20.0 defect class: a
// postgres MAJOR bump is a data migration, not an image bump, because each
// major stores its cluster somewhere the previous one does not look.
//
// Concretely, and measured against postgres:18.6-alpine rather than inferred:
// 16 uses PGDATA=/var/lib/postgresql/data and declares that path as its
// VOLUME; 18 uses PGDATA=/var/lib/postgresql/18/docker and declares the parent
// /var/lib/postgresql. An existing cluster therefore is not where the new
// major looks, and the operator has to dump on the old major and restore into
// the new one (docs/upgrade-guide.md; proven end to end by
// TestPostgres16To18DumpRestoreRoundTrip).
//
// Two details this test exists to keep true, both of which have been wrong in
// this repo before:
//
//   - It covers BOTH compose files. The prod file is the one that carries a
//     persistent volume, but it is excluded from Dependabot by exclude-paths
//     (.github/dependabot.yml), so the dev file is the only one an automated
//     bump can actually reach. Gating prod alone gated the file nothing was
//     going to change.
//   - It covers every service running a postgres image, not just the service
//     named "postgres". The prod stack's "backup" sidecar runs pg_dump against
//     the server, and pg_dump refuses to dump a server newer than itself
//     ("aborting because of server version mismatch", verified against 18.6),
//     so a sidecar left behind stops producing backups entirely.
func TestPostgresMajorIsPinnedEverywhere(t *testing.T) {
	for _, path := range []string{devComposePath, prodComposePath} {
		found := false
		for name, svc := range composeServices(t, path) {
			img, ok := svc["image"].(string)
			if !ok || !strings.HasPrefix(img, "postgres:") {
				continue
			}
			found = true
			tag := img[strings.Index(img, ":")+1:]
			major := strings.SplitN(tag, "-", 2)[0]
			if major != wantPostgresMajor {
				t.Errorf("%s: service %q image %q pins postgres major %q, want %s. A major bump moves the cluster to a directory the other major does not read, so it needs the dump/restore in docs/upgrade-guide.md, the pgdata mount point checked by TestProdPostgresVolumeMountsTheVersionedParent, and this constant moved in the same change",
					path, name, img, major, wantPostgresMajor)
			}
		}
		if !found {
			t.Errorf("%s: no postgres image found", path)
		}
	}
}

// TestProdPostgresVolumeMountsTheVersionedParent pins the half of the major
// bump that the image tag does not carry. postgres 18 keeps its cluster in
// /var/lib/postgresql/<major>/docker and declares /var/lib/postgresql as its
// VOLUME, and its entrypoint refuses to boot if anything is mounted at the
// pre-18 /var/lib/postgresql/data.
//
// Measured against 18.6-alpine, not inferred: that refusal fires even when the
// volume is EMPTY, reported as "/var/lib/postgresql/data (unused mount/volume)"
// with exit 1. So `make smoke-prod` does catch a wrong mount point. This test
// is not the only thing standing between the repo and that mistake. What it
// adds is speed and a reason: it fails in milliseconds without a Docker stack,
// and says which path to use, where smoke-prod reports a container that would
// not start.
func TestProdPostgresVolumeMountsTheVersionedParent(t *testing.T) {
	const want = "orbeat-pgdata:/var/lib/postgresql"

	svc, ok := composeServices(t, prodComposePath)["postgres"]
	if !ok {
		t.Fatal(`service "postgres" missing from the prod compose file`)
	}
	vols, ok := svc["volumes"].([]any)
	if !ok {
		t.Fatal("prod postgres declares no volumes")
	}
	for _, v := range vols {
		s, _ := v.(string)
		if !strings.HasPrefix(s, "orbeat-pgdata:") {
			continue
		}
		if s != want {
			t.Errorf("prod postgres mounts pgdata as %q, want %q. postgres %s stores its cluster in /var/lib/postgresql/%s/docker, so the parent is what has to persist; anything mounted at the pre-18 .../data path makes the entrypoint exit 1, empty volume included",
				s, want, wantPostgresMajor, wantPostgresMajor)
		}
		return
	}
	t.Errorf("prod postgres does not mount the orbeat-pgdata volume at all (volumes: %v). The database would be entirely ephemeral", vols)
}

// TestProdKeycloakHealthcheckProbesRealmDiscovery pins the harder half of
// the v1.20.0 postmortem: in production mode, Keycloak's own /health/ready
// can report UP while a BootstrapFilter still 503s every request (widened
// by 26.7's slower `start` bootstrap plus the realm import), so readiness
// is not "ready to serve". The fix gates depends_on on a 200 from the
// realm's OIDC discovery document instead — see the healthcheck's own
// comment in the compose file.
func TestProdKeycloakHealthcheckProbesRealmDiscovery(t *testing.T) {
	svc, ok := composeServices(t, prodComposePath)["keycloak"]
	if !ok {
		t.Fatal(`service "keycloak" missing from the prod compose file`)
	}
	hc, ok := svc["healthcheck"].(map[string]any)
	if !ok {
		t.Fatal("keycloak has no healthcheck")
	}
	test, ok := hc["test"].([]any)
	if !ok {
		t.Fatal("keycloak healthcheck has no test")
	}
	var cmd string
	for _, part := range test {
		if s, _ := part.(string); s != "" {
			cmd += s + " "
		}
	}
	if strings.Contains(cmd, "/health/ready") {
		t.Errorf("keycloak healthcheck probes /health/ready: %q — readiness can report UP while a BootstrapFilter still 503s every request (v1.20.0); probe realm discovery instead", cmd)
	}
	if !strings.Contains(cmd, "openid-configuration") {
		t.Errorf("keycloak healthcheck = %q, want it to probe the realm's OIDC discovery endpoint (…/.well-known/openid-configuration) so service_healthy means \"ready to serve tokens\"", cmd)
	}
}

// TestProdKeycloakImportsRealmOnBoot pins the v1.20.0 defect: without
// --import-realm the mounted realm JSON is never imported, and the api and
// gateway 404 on OIDC discovery forever.
func TestProdKeycloakImportsRealmOnBoot(t *testing.T) {
	svc, ok := composeServices(t, prodComposePath)["keycloak"]
	if !ok {
		t.Fatal(`service "keycloak" missing from the prod compose file`)
	}
	cmd, ok := svc["command"].([]any)
	if !ok {
		t.Fatal("keycloak has no command")
	}
	found := false
	for _, c := range cmd {
		if s, _ := c.(string); s == "--import-realm" {
			found = true
		}
	}
	if !found {
		t.Errorf("keycloak command %v is missing --import-realm — the realm is never imported and the api/gateway 404 on OIDC discovery (v1.20.0)", cmd)
	}
}

// TestProdKeycloakHostnameBackchannelDynamic pins the v1.20.0 defect: without
// this, the discovery document served at keycloak:8080 returns jwks_uri as
// the PUBLIC https://auth.<domain> URL, which the api/gateway cannot resolve
// inside the compose network — their eager JWKS fetch fails and they never
// become healthy.
func TestProdKeycloakHostnameBackchannelDynamic(t *testing.T) {
	svc, ok := composeServices(t, prodComposePath)["keycloak"]
	if !ok {
		t.Fatal(`service "keycloak" missing from the prod compose file`)
	}
	env, ok := svc["environment"].(map[string]any)
	if !ok {
		t.Fatal("keycloak has no environment map")
	}
	got, ok := env["KC_HOSTNAME_BACKCHANNEL_DYNAMIC"]
	if !ok {
		t.Fatal("keycloak is missing KC_HOSTNAME_BACKCHANNEL_DYNAMIC — jwks_uri would point at the unreachable public host and the api/gateway never become healthy (v1.20.0)")
	}
	if s, _ := got.(string); s != "true" {
		t.Errorf("KC_HOSTNAME_BACKCHANNEL_DYNAMIC = %v, want \"true\"", got)
	}
}

// TestProdKeycloakProxyTrustedAddressesAbsent pins a deliberate omission,
// not a missing setting: KC_PROXY_TRUSTED_ADDRESSES requires an IP/CIDR
// (Keycloak rejects a Docker service name), and Keycloak boots before Caddy
// so a startup lookup has nothing to resolve — see the compose file's own
// comment beside it. The default (trust the forwarding proxy) is safe here
// because Keycloak publishes no host port. A red-proof matters more than
// usual here: asserting a key is absent passes trivially on a file that
// never had it, so this test only proves something if adding the key back
// makes it fail.
func TestProdKeycloakProxyTrustedAddressesAbsent(t *testing.T) {
	svc, ok := composeServices(t, prodComposePath)["keycloak"]
	if !ok {
		t.Fatal(`service "keycloak" missing from the prod compose file`)
	}
	env, ok := svc["environment"].(map[string]any)
	if !ok {
		t.Fatal("keycloak has no environment map")
	}
	if v, ok := env["KC_PROXY_TRUSTED_ADDRESSES"]; ok {
		t.Errorf("KC_PROXY_TRUSTED_ADDRESSES = %v, want unset — it requires an IP/CIDR and Keycloak boots before Caddy so a startup lookup has nothing to resolve (v1.20.0); the default (trust the forwarding proxy) is safe because Keycloak publishes no host port", v)
	}
}

// TestProdGatewayDependsOnAPIHealthy pins the v1.21.0 fix: orbeat-api is the
// only service that applies migrations, and the gateway reads the same
// schema directly via internal/store, so without this edge a plain `up -d`
// may start the gateway against a not-yet-migrated database.
func TestProdGatewayDependsOnAPIHealthy(t *testing.T) {
	svc, ok := composeServices(t, prodComposePath)["gateway"]
	if !ok {
		t.Fatal(`service "gateway" missing from the prod compose file`)
	}
	dep, ok := svc["depends_on"].(map[string]any)
	if !ok {
		t.Fatal("gateway has no depends_on map (or it uses the list form, which cannot express a healthy condition)")
	}
	api, ok := dep["api"].(map[string]any)
	if !ok {
		t.Fatal("gateway does not depend on api")
	}
	if cond, _ := api["condition"].(string); cond != "service_healthy" {
		t.Errorf("gateway depends_on.api.condition = %q, want service_healthy — api's healthcheck is the only condition that means \"schema is current\" (v1.21.0)", cond)
	}
}

// TestProdNamedVolumesExist pins v1.20.0's first-ever persistent volumes.
// A dropped volume definition doesn't fail `docker compose config -q`; it
// silently makes the stack ephemeral, losing pgdata, backups and the
// published marketplace on every recreate.
func TestProdNamedVolumesExist(t *testing.T) {
	var doc struct {
		Volumes map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal(repoFile(t, prodComposePath), &doc); err != nil {
		t.Fatalf("parse prod compose: %v", err)
	}
	for _, name := range []string{"orbeat-pgdata", "orbeat-backups", "orbeat-marketplace", "caddy-data"} {
		if _, ok := doc.Volumes[name]; !ok {
			t.Errorf("named volume %q is missing — v1.20.0's first-ever persistent volume being dropped would silently make the stack ephemeral", name)
		}
	}
}

// prodOptionalEnvVars are the vars the prod compose file deliberately lets
// float with a "${VAR:-default}" default, because each has a documented,
// safe operational default when unset (see the comment beside its
// definition in deploy/docker-compose.prod.yml). Every other interpolated
// variable in the file is a required credential or required deployment
// parameter (POSTGRES_PASSWORD, the Keycloak bootstrap admin pair,
// ORBEAT_DOMAIN, ACME_EMAIL) and must use the "${VAR:?…}" required form —
// unset should refuse to start compose, not silently render empty.
var prodOptionalEnvVars = map[string]bool{
	"ORBEAT_BACKUP_INTERVAL":    true,
	"ORBEAT_BACKUP_KEEP":        true,
	"ORBEAT_RATELIMIT_RPS":      true,
	"ORBEAT_RATELIMIT_BURST":    true,
	"ORBEAT_RATELIMIT_INIT_RPS": true,
}

// envVarInterpolation matches compose's "${VAR}", "${VAR:-default}" and
// "${VAR:?message}" interpolation forms so TestProdSecretsRequireExplicitValue
// can classify every one found in the raw file text, independent of which
// YAML field it sits in (environment, healthcheck test, command, ...).
var envVarInterpolation = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)(:[?-][^}]*)?\}`)

// TestProdSecretsRequireExplicitValue pins that an unset required var
// refuses to start compose rather than silently rendering as an empty
// string — the difference between POSTGRES_PASSWORD being unset failing
// loudly at `docker compose up` versus Postgres booting with an empty
// password. It scans the raw file text rather than the parsed YAML because
// the interpolation syntax is opaque to YAML — compose expands it at
// runtime, not parse time — so a struct-typed walk would have to visit
// every field by hand and would silently miss the next one added.
func TestProdSecretsRequireExplicitValue(t *testing.T) {
	raw := string(repoFile(t, prodComposePath))
	for _, m := range envVarInterpolation.FindAllStringSubmatch(raw, -1) {
		name, op := m[1], m[2]
		if prodOptionalEnvVars[name] {
			continue
		}
		if !strings.HasPrefix(op, ":?") {
			t.Errorf("%q is interpolated as %q, want the ${%s:?…} required form — an unset value would silently render empty instead of compose refusing to start", name, m[0], name)
		}
	}
}
