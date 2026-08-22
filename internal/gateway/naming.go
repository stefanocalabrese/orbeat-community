// Package gateway implements orbeat's MCP gateway broker.
package gateway

import (
	"strings"
)

// sep separates the server slug from the tool name in a namespaced tool id.
const sep = "__"

// Namespace builds the gateway-facing tool id "<slug>__<tool>". Precondition:
// slug must be non-empty (callers enforce this — an empty slug yields a malformed
// "__<tool>" id). naming.Slugify can return "" for all-punctuation input.
func Namespace(slug, tool string) string {
	return slug + sep + tool
}

// Split reverses Namespace, splitting on the FIRST separator so tool names may
// contain "__". ok is false if no separator is present.
func Split(name string) (slug, tool string, ok bool) {
	i := strings.Index(name, sep)
	if i < 0 {
		return "", "", false
	}
	return name[:i], name[i+len(sep):], true
}
