package syncclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestFatalRoundTrip(t *testing.T) {
	base := errors.New("boom")
	f := markFatal(base)
	if !isFatal(f) {
		t.Fatal("markFatal result not detected by isFatal")
	}
	if f.Error() != "boom" {
		t.Fatalf("message changed: got %q, want %q", f.Error(), "boom")
	}
	if !errors.Is(f, base) {
		t.Fatal("Unwrap broken: errors.Is(markFatal(base), base) is false")
	}
	if isFatal(base) {
		t.Fatal("a plain error must not be fatal")
	}
	if markFatal(nil) != nil {
		t.Fatal("markFatal(nil) must return nil")
	}
}

func TestIsFatalThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("context: %w", markFatal(errors.New("inner")))
	if !isFatal(wrapped) {
		t.Fatal("isFatal must see a fatal error through %w wrapping")
	}
}

func TestLoadManifestParseErrorIsFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadManifest(dir)
	if err == nil || !isFatal(err) {
		t.Fatalf("corrupt manifest: want fatal error, got %v", err)
	}
}

func TestResolveContainedEscapeIsFatal(t *testing.T) {
	_, err := resolveContained(t.TempDir(), "../evil")
	if err == nil || !isFatal(err) {
		t.Fatalf("path escape: want fatal error, got %v", err)
	}
}
