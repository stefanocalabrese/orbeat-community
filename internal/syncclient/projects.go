package syncclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Managed project list for project-scope seed targeting (Slice B §7.4).
// Persisted separately from Config (which is env-derived and never written):
// only credentials.json and this file live on disk.

type projectsFile struct {
	Projects []string `json:"projects"`
}

// DefaultProjectsPath is ~/.config/orbeat/projects.json — a sibling of the
// Slice-A token store credentials.json, in the same 0700 directory.
func DefaultProjectsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("projects path: %w", err)
	}
	return filepath.Join(home, ".config", "orbeat", "projects.json"), nil
}

// LoadProjects reads the registered project paths (empty when the file is absent).
func LoadProjects(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("load projects: %w", err)
	}
	var f projectsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("load projects: parse: %w", err)
	}
	return f.Projects, nil
}

// saveProjects writes the list atomically. The parent dir is created 0700
// first (it also holds credentials.json), so writeFileAtomic's 0755 MkdirAll
// is a no-op that never widens it.
func saveProjects(path string, list []string) error {
	sort.Strings(list)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save projects: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(projectsFile{Projects: list}, "", "  ")
	if err != nil {
		return fmt.Errorf("save projects: marshal: %w", err)
	}
	return writeFileAtomic(path, data, 0o644)
}

// AddProject registers an absolute, existing directory (stored cleaned;
// idempotent). Returns the cleaned absolute path that was stored.
func AddProject(path, proj string) (string, error) {
	abs, err := filepath.Abs(proj)
	if err != nil {
		return "", fmt.Errorf("add project: %w", err)
	}
	abs = filepath.Clean(abs)
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("add project: %s: %w", abs, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("add project: %s is not a directory", abs)
	}
	list, err := LoadProjects(path)
	if err != nil {
		return "", err
	}
	for _, p := range list {
		if p == abs {
			return abs, nil
		}
	}
	return abs, saveProjects(path, append(list, abs))
}

// RemoveProject unregisters proj. Returns the cleaned path and whether it was
// registered. Callers strip the project's seed blocks separately (spec §7.4);
// if that strip is ever missed, the next sync's ledger-driven pass self-heals.
func RemoveProject(path, proj string) (string, bool, error) {
	abs, err := filepath.Abs(proj)
	if err != nil {
		return "", false, fmt.Errorf("remove project: %w", err)
	}
	abs = filepath.Clean(abs)
	list, err := LoadProjects(path)
	if err != nil {
		return abs, false, err
	}
	kept := make([]string, 0, len(list))
	found := false
	for _, p := range list {
		if p == abs {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		return abs, false, nil
	}
	return abs, true, saveProjects(path, kept)
}
