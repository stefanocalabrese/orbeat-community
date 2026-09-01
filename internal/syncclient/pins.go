package syncclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Pin is one artifact this machine holds at a specific revision, as recorded
// in pins.json. ArtifactID is authoritative; Type and Name are a label
// written once at pin time, for 'orbeat-sync pin list' and for the warning
// lines a sync prints when this pin cannot be honoured: they are never
// re-resolved against a rename, the same way the rendered file path is not.
type Pin struct {
	ArtifactID string `json:"artifactId"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	Revision   int    `json:"revision"`
}

type pinsFile struct {
	Pins []Pin `json:"pins"`
}

// DefaultPinsPath is ~/.config/orbeat/pins.json, a sibling of
// credentials.json / projects.json / connect.json / install.json in the same
// 0700 state dir.
func DefaultPinsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("pins path: %w", err)
	}
	return filepath.Join(home, ".config", "orbeat", "pins.json"), nil
}

// LoadPins reads the pin file.
//
// An ABSENT file is (nil, nil), not an error, mirroring LoadInstallID's own
// contract for the same reason: a machine that has never run
// 'orbeat-sync pin' has none, and that must read as "no pins", not as a
// failure every other command then has to route around.
func LoadPins(path string) ([]Pin, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("load pins: %w", err)
	}
	var f pinsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("load pins: parse %s: %w", path, err)
	}
	return f.Pins, nil
}

// savePins writes the pin list atomically, sorted by artifact id so two runs
// that hold the identical set produce byte-identical output, the same
// determinism saveProjects gives the projects list. 0600, mirroring
// credentials.json/install.json rather than projects.json's 0644: a pin file
// carries no secret, but the tighter mode costs nothing and keeps the state
// dir's permission story uniform for every file nothing else needs to read.
func savePins(path string, pins []Pin) error {
	sort.Slice(pins, func(i, j int) bool { return pins[i].ArtifactID < pins[j].ArtifactID })
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save pins: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(pinsFile{Pins: pins}, "", "  ")
	if err != nil {
		return fmt.Errorf("save pins: marshal: %w", err)
	}
	return writeFileAtomic(path, data, 0o600)
}

// SetPin writes pin, replacing any existing pin for the same artifact id.
// Sync never calls this: a pin changes only through an explicit command,
// which is what makes pins.json the developer's own state rather than
// something orbeat mutates behind her (see cmd/sync's runPinSet).
func SetPin(path string, pin Pin) error {
	pins, err := LoadPins(path)
	if err != nil {
		return err
	}
	out := make([]Pin, 0, len(pins)+1)
	replaced := false
	for _, p := range pins {
		if p.ArtifactID == pin.ArtifactID {
			out = append(out, pin)
			replaced = true
			continue
		}
		out = append(out, p)
	}
	if !replaced {
		out = append(out, pin)
	}
	return savePins(path, out)
}

// RemovePin removes the pin labelled typ/name, matched on the label rather
// than an artifact id the caller of 'orbeat-sync pin remove' never has in
// hand. Reports whether a pin was actually found and removed.
func RemovePin(path, typ, name string) (bool, error) {
	pins, err := LoadPins(path)
	if err != nil {
		return false, err
	}
	kept := make([]Pin, 0, len(pins))
	found := false
	for _, p := range pins {
		if p.Type == typ && p.Name == name {
			found = true
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		return false, nil
	}
	return true, savePins(path, kept)
}
