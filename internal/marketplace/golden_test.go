package marketplace

import (
	"os"
	"path/filepath"
	"testing"
)

// The committed dev-default marketplace lives at the repo root (../../marketplace
// relative to this package). It must be byte-for-byte reproducible from the
// generator with the dev gateway URL; drift fails CI. Regenerate with
// `make marketplace` after changing the generator.
func TestCommittedMarketplaceMatchesGenerator(t *testing.T) {
	const devGatewayURL = "http://localhost:8090"
	committed := filepath.Join("..", "..", "marketplace")

	gen := t.TempDir()
	if err := Generate(gen, Options{GatewayURL: devGatewayURL}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	for _, rel := range relFiles() {
		want := mustRead(t, filepath.Join(gen, rel))
		got, err := os.ReadFile(filepath.Join(committed, rel))
		if err != nil {
			t.Fatalf("committed marketplace missing %s (%v); run `make marketplace`", rel, err)
		}
		if string(got) != string(want) {
			t.Errorf("committed %s is stale; run `make marketplace`", rel)
		}
	}
}
