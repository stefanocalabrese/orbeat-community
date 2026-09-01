package main

import "github.com/stefanocalabrese/orbeat-community/internal/syncclient"

// projs builds untagged Projects from paths; see the sibling helper in
// internal/syncclient for why untagged is the right default here.
func projs(paths ...string) []syncclient.Project {
	out := make([]syncclient.Project, 0, len(paths))
	for _, p := range paths {
		out = append(out, syncclient.Project{Path: p})
	}
	return out
}
