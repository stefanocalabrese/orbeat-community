// Package naming holds name-normalization helpers shared across services:
// the gateway derives tool-namespace slugs from server names, and the api
// rejects catalog writes whose names would collide after slugification.
package naming

import (
	"regexp"
	"strings"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify reduces s to the safe tool-name charset [a-z0-9-]: lowercased,
// runs of other characters collapsed to a single '-', trimmed. The gateway's
// reserved separator "__" cannot appear in a slug (its underscores collapse
// to '-'). Slugify is lossy: distinct names ("My Server!", "my server") can
// share a slug — the api's collision check and the gateway's build-time slug
// guard exist because of exactly that.
func Slugify(s string) string {
	out := nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(out, "-")
}
