package syncclient

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPinsSetRemoveListRoundTrip(t *testing.T) {
	dir := t.TempDir()
	pp := filepath.Join(dir, "cfg", "pins.json")

	// Empty state: no file -> empty list, no error.
	pins, err := LoadPins(pp)
	if err != nil || len(pins) != 0 {
		t.Fatalf("empty load: %v %v", err, pins)
	}

	// SetPin creates the file and the entry.
	if err := SetPin(pp, Pin{ArtifactID: "id-a", Type: "skill", Name: "a", Revision: 3}); err != nil {
		t.Fatalf("set a: %v", err)
	}
	pins, err = LoadPins(pp)
	if err != nil || len(pins) != 1 || pins[0] != (Pin{ArtifactID: "id-a", Type: "skill", Name: "a", Revision: 3}) {
		t.Fatalf("after set a: %v %+v", err, pins)
	}

	// SetPin on a second artifact adds, not replaces.
	if err := SetPin(pp, Pin{ArtifactID: "id-b", Type: "subagent", Name: "b", Revision: 7}); err != nil {
		t.Fatalf("set b: %v", err)
	}
	pins, err = LoadPins(pp)
	if err != nil || len(pins) != 2 {
		t.Fatalf("after set b: %v %+v", err, pins)
	}

	// SetPin on the SAME artifact id replaces the existing entry, not appends.
	if err := SetPin(pp, Pin{ArtifactID: "id-a", Type: "skill", Name: "a", Revision: 5}); err != nil {
		t.Fatalf("re-set a: %v", err)
	}
	pins, err = LoadPins(pp)
	if err != nil || len(pins) != 2 {
		t.Fatalf("after re-set a: %v %+v", err, pins)
	}
	var gotA Pin
	for _, p := range pins {
		if p.ArtifactID == "id-a" {
			gotA = p
		}
	}
	if gotA.Revision != 5 {
		t.Fatalf("re-set a did not replace: %+v", pins)
	}

	// RemovePin matches by label (type/name), reports found, and leaves the
	// other pin untouched.
	found, err := RemovePin(pp, "skill", "a")
	if err != nil || !found {
		t.Fatalf("remove a: %v found=%v", err, found)
	}
	pins, err = LoadPins(pp)
	if err != nil || len(pins) != 1 || pins[0].ArtifactID != "id-b" {
		t.Fatalf("after remove a: %v %+v", err, pins)
	}

	// A second remove of the same label reports !found and changes nothing.
	found, err = RemovePin(pp, "skill", "a")
	if err != nil || found {
		t.Fatalf("second remove of a: %v found=%v", err, found)
	}

	// The config dir must be private (0700), like the token store's.
	st, err := os.Stat(filepath.Dir(pp))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("config dir perms: %v %v", err, st.Mode())
	}
}

func TestLoadPinsAbsentIsNotAnError(t *testing.T) {
	pins, err := LoadPins(filepath.Join(t.TempDir(), "nope", "pins.json"))
	if err != nil {
		t.Fatalf("absent pins.json must not be an error, got %v", err)
	}
	if pins != nil {
		t.Fatalf("absent pins.json must decode to nil, got %+v", pins)
	}
}

func TestLoadPinsRejectsUnparseableJSON(t *testing.T) {
	dir := t.TempDir()
	pp := filepath.Join(dir, "pins.json")
	if err := os.WriteFile(pp, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPins(pp); err == nil {
		t.Fatal("want an error for unparseable pins.json")
	}
}
