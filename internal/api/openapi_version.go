package api

import (
	"bytes"
	"fmt"

	"github.com/stefanocalabrese/orbeat-community/internal/version"
)

// openapiVersionPlaceholder is the literal info.version line embedded in
// openapi.yaml. openapi.yaml is a static YAML file, not a Go binary — it
// cannot take the -ldflags -X substitution release.yml uses for
// internal/version.Version, and adding a hand-edited "update the spec's
// version" release step is exactly the kind of step this repo's release
// process has already dropped twice (CLAUDE.md's Versioning bullet). So the
// real version is substituted here, at serve time, from the single shared
// internal/version.Version — and the embedded file keeps an obviously-fake
// placeholder rather than a stale-but-plausible-looking number, so a broken
// substitution is visible immediately rather than shipping quietly wrong.
const openapiVersionPlaceholder = "version: 0.0.0-UNSET"

// renderedOpenAPISpec returns the embedded OpenAPI document with its
// info.version substituted for the given version string. It panics if the
// placeholder is missing from the embedded file — a byte-identical
// substitution target is exactly as gateable as a compiled linker symbol
// path, and TestRenderedOpenAPISpecRequiresPlaceholder proves this branch is
// reachable rather than dead code.
func renderedOpenAPISpec(v string) []byte {
	marker := []byte(openapiVersionPlaceholder)
	if !bytes.Contains(openapiSpec, marker) {
		panic(fmt.Sprintf("internal/api: openapi.yaml is missing the version placeholder %q — "+
			"GET /openapi.yaml would otherwise serve a version nobody substituted", openapiVersionPlaceholder))
	}
	return bytes.Replace(openapiSpec, marker, []byte("version: "+v), 1)
}

// currentOpenAPISpec is renderedOpenAPISpec resolved against the process's
// live internal/version.Version, called fresh on every request (not cached
// at package init) so a test that flips version.Version observes the change,
// matching how internal/gateway's gatewayImplementation resolves it.
func currentOpenAPISpec() []byte {
	return renderedOpenAPISpec(version.Version)
}
