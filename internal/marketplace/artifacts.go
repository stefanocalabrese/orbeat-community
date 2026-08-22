package marketplace

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// artifactNameRe mirrors the API's slugRe (internal/api/admin_artifacts.go)
// that gates artifact creation/update. This is a deliberate second check: the
// API validates on the write path, but the renderer builds on-disk paths
// straight from a.Name (audit G12) — it must hold the slug invariant on its
// own, independent of the API boundary, so a direct DB write (a migration, an
// admin SQL fix, a future API bug) can never smuggle a path-traversal or
// absolute-path segment into a published file path.
var artifactNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Artifact is the renderer's input for a distributable artifact (decoupled from
// the store type). MemoryScope applies to subagents ("" = none).
type Artifact struct {
	Type        string // skill | subagent
	Name        string
	Content     string
	MemoryScope string
}

const ArtifactsPluginName = "orbeat-artifacts"

// ValidateArtifactContent ensures the content has YAML frontmatter with the
// required name + description keys.
func ValidateArtifactContent(content string) error {
	fm, _, err := splitFrontmatter(content)
	if err != nil {
		return err
	}
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		return fmt.Errorf("marketplace: frontmatter is not valid YAML: %w", err)
	}
	if s, _ := m["name"].(string); strings.TrimSpace(s) == "" {
		return fmt.Errorf("marketplace: frontmatter missing required 'name'")
	}
	if s, _ := m["description"].(string); strings.TrimSpace(s) == "" {
		return fmt.Errorf("marketplace: frontmatter missing required 'description'")
	}
	return nil
}

// splitFrontmatter returns the YAML frontmatter block and the body. Errors if
// the content does not start with a `---` fenced frontmatter block. Line
// endings are normalized to LF; the closing fence must be a line that is
// exactly `---`.
func splitFrontmatter(content string) (frontmatter, body string, err error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return "", "", fmt.Errorf("marketplace: content missing leading '---' frontmatter")
	}
	rest := content[len("---\n"):]
	for from := 0; ; {
		i := strings.Index(rest[from:], "\n---")
		if i < 0 {
			return "", "", fmt.Errorf("marketplace: unterminated frontmatter block")
		}
		abs := from + i
		after := rest[abs+len("\n---"):]     // bytes after the "\n---"
		if after == "" || after[0] == '\n' { // closing fence is a full `---` line (or EOF)
			return rest[:abs], strings.TrimPrefix(after, "\n"), nil
		}
		from = abs + len("\n---")
	}
}

// withMemory re-emits an agent's content with `memory: <scope>` set in the
// frontmatter (overriding any existing memory key). Frontmatter is re-marshalled
// (keys sorted) — deterministic for golden tests.
func withMemory(content, scope string) (string, error) {
	fm, body, err := splitFrontmatter(content)
	if err != nil {
		return "", err
	}
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
		return "", fmt.Errorf("marketplace: frontmatter is not valid YAML: %w", err)
	}
	m["memory"] = scope
	out, err := yaml.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marketplace: re-marshal frontmatter: %w", err)
	}
	return "---\n" + string(out) + "---\n" + body, nil
}

type artifactsPluginManifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Author      ownerRef `json:"author"`
}

// RenderArtifactsPlugin emits the orbeat-artifacts plugin tree (relative paths →
// content). Skills are verbatim; subagents get memory injected when set.
// Returns an error if an artifact's Name is not a valid slug (audit G12): the
// renderer refuses to let it flow into an on-disk path.
func RenderArtifactsPlugin(artifacts []Artifact) (map[string]string, error) {
	base := "plugins/" + ArtifactsPluginName
	files := map[string]string{}
	pm := artifactsPluginManifest{
		Name: ArtifactsPluginName, Version: PluginVersion,
		Description: "orbeat governed skills and subagents.", Author: ownerRef{Name: ownerName},
	}
	files[base+"/.claude-plugin/plugin.json"] = mustJSON(pm)
	for _, a := range artifacts {
		switch a.Type {
		case "skill":
			if !artifactNameRe.MatchString(a.Name) {
				return nil, fmt.Errorf("marketplace: artifact name %q is not a valid slug", a.Name)
			}
			files[base+"/skills/"+a.Name+"/SKILL.md"] = a.Content
		case "subagent":
			if !artifactNameRe.MatchString(a.Name) {
				return nil, fmt.Errorf("marketplace: artifact name %q is not a valid slug", a.Name)
			}
			content := a.Content
			if a.MemoryScope != "" {
				// withMemory errors here would mean invalid frontmatter that
				// ValidateArtifactContent should have rejected on save; fall back to
				// verbatim rather than failing the whole publish, but surface the
				// silent drop (audit G12) so it is not truly silent.
				if c, err := withMemory(a.Content, a.MemoryScope); err == nil {
					content = c
				} else {
					slog.Warn("marketplace: memory injection failed, publishing content verbatim",
						"artifact", a.Name, "scope", a.MemoryScope, "err", err)
				}
			}
			files[base+"/agents/"+a.Name+".md"] = content
		default:
			// unknown type — skipped; Type is constrained at the DB + API layer.
		}
	}
	return files, nil
}

// RenderArtifactContent returns the final on-disk file body for a single artifact:
// skills verbatim; subagents with `memory: <scope>` injected when MemoryScope is set
// (reusing withMemory). The authenticated sync endpoint (Channel-2) returns this so
// the client writes content verbatim — keeping frontmatter logic server-side.
//
// Unlike RenderArtifactsPlugin, this does NOT slug-assert a.Name: it returns only the
// file body, never a path. The Channel-2 client independently re-validates the name
// against the same slug rule before building any on-disk path (internal/syncclient/
// reconcile.go), so the filesystem-path chokepoint stays guarded on this channel too.
func RenderArtifactContent(a Artifact) string {
	if a.Type == "subagent" && a.MemoryScope != "" {
		// A withMemory error means invalid frontmatter that ValidateArtifactContent
		// should have rejected on save; fall back to verbatim rather than failing,
		// but surface the silent drop (audit G12).
		c, err := withMemory(a.Content, a.MemoryScope)
		if err == nil {
			return c
		}
		slog.Warn("marketplace: memory injection failed, syncing content verbatim",
			"artifact", a.Name, "scope", a.MemoryScope, "err", err)
	}
	return a.Content
}

// RenderMarketplace composes the full published tree: the orbeat-gateway connect
// plugin + the orbeat-artifacts plugin, with marketplace.json listing both.
func RenderMarketplace(opts Options, artifacts []Artifact) (map[string]string, error) {
	files := map[string]string{}
	// connect plugin (reuse existing single-plugin rendering)
	for p, c := range renderConnectPlugin(opts) {
		files[p] = c
	}
	artFiles, err := RenderArtifactsPlugin(artifacts)
	if err != nil {
		return nil, err
	}
	for p, c := range artFiles {
		files[p] = c
	}
	files[".claude-plugin/marketplace.json"] = renderMarketplaceManifest(len(artifacts) > 0)
	return files, nil
}
