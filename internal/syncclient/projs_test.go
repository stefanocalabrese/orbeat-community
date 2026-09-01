package syncclient

// projs builds untagged Projects from paths, for the many rules tests written
// before targeting existed. Untagged is the right default for them: a rule with
// no target tags applies to every project, so those tests keep asserting
// exactly what they always asserted.
func projs(paths ...string) []Project {
	out := make([]Project, 0, len(paths))
	for _, p := range paths {
		out = append(out, Project{Path: p})
	}
	return out
}
