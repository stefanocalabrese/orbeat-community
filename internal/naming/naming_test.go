package naming

import "testing"

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"github":        "github",
		"GitHub":        "github",
		"my server!":    "my-server",
		"a__b":          "a-b", // collapse the gateway's reserved separator out of slugs
		"  spaces  ":    "spaces",
		"weird.name/v2": "weird-name-v2",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Fatalf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlugifyEmptyAndPunctuation(t *testing.T) {
	for _, in := range []string{"", "!!!", "   ", "___"} {
		if got := Slugify(in); got != "" {
			t.Fatalf("Slugify(%q) = %q, want empty", in, got)
		}
	}
}
