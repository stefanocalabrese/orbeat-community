package marketplace

import (
	"path/filepath"
	"testing"
)

// The committed dev-default marketplace lives at the repo root (../../marketplace
// relative to this package). It must be byte-for-byte reproducible from the
// generator with the dev gateway URL; drift fails CI. Regenerate with
// `make marketplace` after changing the generator.
//
// The file SET compared is derived from what Generate actually wrote into a
// fresh temp dir (filesUnder, marketplace_test.go) rather than a hardcoded
// list of relative paths -- a fixed list only ever proves the files someone
// remembered to name in it stayed in sync, and says nothing about a fourth
// file the renderer starts (or stops) writing.
//
// Both directions are checked, because they are two different ways this can
// go stale and Generate does not clean its output directory before writing
// (marketplace.go's Generate, and `make marketplace`'s plain `go run
// ./cmd/marketplacegen`), which is exactly what makes the second direction
// reachable in practice, not just in theory: a file the renderer used to
// write and later stopped writing survives in the committed tree forever,
// silent, unless something walks the committed directory too and asks
// whether each entry is still something Generate produces.
//
//  1. Every file Generate renders must exist in the committed tree with
//     identical bytes (the direction the old hardcoded loop covered).
//  2. Every file in the committed tree must be something Generate still
//     renders (the direction nothing here covered before: a stale leftover
//     file was invisible to a loop that only ever walks the renderer's own
//     output, since it can only ask about files it already expects).
func TestCommittedMarketplaceMatchesGenerator(t *testing.T) {
	const devGatewayURL = "http://localhost:8090"
	committed := filepath.Join("..", "..", "marketplace")

	gen := t.TempDir()
	if err := Generate(gen, Options{GatewayURL: devGatewayURL}); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	rendered := filesUnder(t, gen)
	if len(rendered) == 0 {
		t.Fatal("Generate wrote zero files into a fresh temp dir; every assertion below would " +
			"compare an empty rendered set and pass over nothing")
	}
	golden := filesUnder(t, committed)
	if len(golden) == 0 {
		t.Fatal("the committed marketplace/ tree contains zero files; either it was deleted or " +
			"filesUnder stopped walking it, and every assertion below would compare an empty " +
			"golden set and pass over nothing")
	}

	inGolden := map[string]bool{}
	for _, rel := range golden {
		inGolden[rel] = true
	}
	for _, rel := range rendered {
		if !inGolden[rel] {
			t.Errorf("Generate renders %s but the committed marketplace/ tree does not contain "+
				"it; run `make marketplace`", rel)
			continue
		}
		want := mustRead(t, filepath.Join(gen, rel))
		got := mustRead(t, filepath.Join(committed, rel))
		if string(got) != string(want) {
			t.Errorf("committed %s is stale; run `make marketplace`", rel)
		}
	}

	inRendered := map[string]bool{}
	for _, rel := range rendered {
		inRendered[rel] = true
	}
	for _, rel := range golden {
		if !inRendered[rel] {
			t.Errorf("committed marketplace/%s exists but Generate no longer renders it; this "+
				"file will never be regenerated or updated by `make marketplace` (Generate does "+
				"not clean its output directory) -- delete it from the committed tree", rel)
		}
	}
}
