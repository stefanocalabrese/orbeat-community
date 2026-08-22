package syncclient

import "testing"

func TestPlanRecordsOpsInOrder(t *testing.T) {
	var p Plan
	p.recordWrite("/root/b.md", false) // overwrite
	p.recordWrite("/root/a.md", true)  // create
	p.recordRemove("/root/c.md")       // remove
	p.recordWrite("/root/d.md", false) // overwrite

	got := p.Changes()
	want := []Change{
		{Path: "/root/b.md", Op: OpOverwrite},
		{Path: "/root/a.md", Op: OpCreate},
		{Path: "/root/c.md", Op: OpRemove},
		{Path: "/root/d.md", Op: OpOverwrite},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d changes, got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change %d: want %+v, got %+v", i, want[i], got[i])
		}
	}
}

func TestNilPlanIsInactive(t *testing.T) {
	var p *Plan
	if p.active() {
		t.Fatal("a nil *Plan must report inactive — that is what keeps the real path unchanged")
	}
}

func TestPlanActiveWhenNonNil(t *testing.T) {
	if !(&Plan{}).active() {
		t.Fatal("a non-nil *Plan must be active — otherwise every dry run silently performs a real sync")
	}
}

func TestChangesNilVsEmpty(t *testing.T) {
	var nilPlan *Plan
	if got := nilPlan.Changes(); got != nil {
		t.Errorf("nil *Plan: want nil (this section never ran), got %+v", got)
	}

	active := &Plan{}
	got := active.Changes()
	if got == nil {
		t.Fatal("active *Plan with nothing recorded: want a non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("active *Plan with nothing recorded: want 0 changes, got %d: %+v", len(got), got)
	}
}
