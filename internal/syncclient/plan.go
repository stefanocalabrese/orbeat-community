package syncclient

// Op is what a dry run says it would do to one path.
type Op string

const (
	OpCreate    Op = "create"
	OpOverwrite Op = "overwrite"
	OpRemove    Op = "remove"
)

// Change is one intended mutation, as a dry run would report it.
type Change struct {
	Path string `json:"path"`
	Op   Op     `json:"op"`
}

// Plan collects the mutations a reconciler WOULD perform. A nil *Plan means
// "not planning" — the real write path — so every call site can pass one
// unconditionally and the default behaviour is unchanged.
//
// Recording order is preservation order: the reconcilers visit paths in a
// deterministic order and the report reads better when it matches what a real
// run would do first.
type Plan struct {
	changes []Change
}

// active reports whether writes should be recorded instead of performed.
// Nil-safe on purpose: `var p *Plan` is the real path.
func (p *Plan) active() bool { return p != nil }

// recordWrite appends an intended write. Assumes a non-nil receiver: call
// only when active() is true. A nil receiver panics by design — see Plan.
func (p *Plan) recordWrite(path string, create bool) {
	op := OpOverwrite
	if create {
		op = OpCreate
	}
	p.changes = append(p.changes, Change{Path: path, Op: op})
}

// recordRemove appends an intended removal. Assumes a non-nil receiver: call
// only when active() is true. A nil receiver panics by design — see Plan.
func (p *Plan) recordRemove(path string) {
	p.changes = append(p.changes, Change{Path: path, Op: OpRemove})
}

// Changes returns the recorded mutations. A nil *Plan yields nil, meaning
// "this section never ran" (the real, non-planning path). A non-nil Plan
// always yields a non-nil slice, even with zero recorded changes, so callers
// can distinguish "did not run" from "ran and changed nothing" — the same
// nil-vs-empty convention strs() applies in cmd/sync/outcome.go.
func (p *Plan) Changes() []Change {
	if p == nil {
		return nil
	}
	if p.changes == nil {
		return []Change{}
	}
	return p.changes
}
