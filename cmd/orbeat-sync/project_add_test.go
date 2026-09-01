package main

import "testing"

// TestParseProjectAdd covers the flag parsing directly, because the failure it
// exists to prevent is silent. `orbeat-sync connect codex` used to connect every
// tool by ignoring its argument; a typo'd `--tags go` here would register a
// project whose targeting matches nothing, and nothing downstream can tell that
// apart from a rule that simply does not apply.
func TestParseProjectAdd(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantPath string
		wantTags []string
		wantErr  bool
	}{
		{"path only", []string{"/p"}, "/p", nil, false},
		{"one tag after the path", []string{"/p", "--tag", "go"}, "/p", []string{"go"}, false},
		{"tags before the path", []string{"--tag", "go", "/p"}, "/p", []string{"go"}, false},
		{"repeated", []string{"/p", "--tag", "go", "--tag", "api"}, "/p", []string{"go", "api"}, false},
		{"equals form", []string{"/p", "--tag=go"}, "/p", []string{"go"}, false},
		{"no path", []string{"--tag", "go"}, "", nil, true},
		{"no args", nil, "", nil, true},
		{"dangling --tag", []string{"/p", "--tag"}, "", nil, true},
		{"two paths", []string{"/p", "/q"}, "", nil, true},
		{"typo'd flag is refused, never ignored", []string{"/p", "--tags", "go"}, "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, tags, err := parseProjectAdd(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got path=%q tags=%v", path, tags)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if path != tc.wantPath {
				t.Fatalf("path = %q, want %q", path, tc.wantPath)
			}
			if len(tags) != len(tc.wantTags) {
				t.Fatalf("tags = %v, want %v", tags, tc.wantTags)
			}
			for i := range tags {
				if tags[i] != tc.wantTags[i] {
					t.Fatalf("tags = %v, want %v", tags, tc.wantTags)
				}
			}
		})
	}
}
