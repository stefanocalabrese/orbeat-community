package syncclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LedgerEntry records what orbeat-sync wrote for one tool.
type LedgerEntry struct {
	MCPPath string `json:"mcp_path"`
}

// ConnectLedger maps adapter name → what was written, so --remove strips exactly
// what connect added and WriteMCP can tell an orbeat-authored entry from a
// user-authored one (the non-clobber check).
type ConnectLedger map[string]LedgerEntry

// DefaultConnectLedgerPath is ~/.config/orbeat/connect.json — a sibling of
// credentials.json / projects.json in the same 0700 state dir.
func DefaultConnectLedgerPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("connect ledger path: %w", err)
	}
	return filepath.Join(home, ".config", "orbeat", "connect.json"), nil
}

// LoadConnectLedger reads the ledger, returning an empty (non-nil) map when the
// file is absent so callers can index/assign without a nil check.
func LoadConnectLedger(path string) (ConnectLedger, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ConnectLedger{}, nil
		}
		return nil, fmt.Errorf("load connect ledger: %w", err)
	}
	l := ConnectLedger{}
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("load connect ledger: parse: %w", err)
	}
	return l, nil
}

// saveConnectLedger writes the ledger atomically (0600 file). The parent 0700
// dir is created by writeFileAtomic if missing (0755) — acceptable here; the
// token store already establishes the tighter 0700 on login.
func saveConnectLedger(path string, l ConnectLedger) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("save connect ledger: %w", err)
	}
	return writeFileAtomic(path, data, 0o600)
}

// ConnectOptions drives a connect run.
type ConnectOptions struct {
	GatewayURL string   // required unless Remove
	LedgerPath string   // required
	Only       []string // --tools: restrict to these adapter names (must all be known)
	Exclude    []string // --exclude: drop these from the selected set
	DryRun     bool     // --dry-run: compute results, write nothing
	Remove     bool     // --remove: strip the orbeat entry instead of writing it

	adapters []ToolAdapter // test seam; nil → allAdapters()
}

// ToolResult is one adapter's outcome plus its adapter for hints/caveats.
type ToolResult struct {
	Tool    string
	Result  Result
	Adapter ToolAdapter
}

// RunConnect selects adapters (default: installed; or Only; minus Exclude),
// writes or removes the orbeat-gateway entry via each, and updates the ledger.
// The default (auto-detect) selection never touches an uninstalled tool; an
// explicit Only list targets the named tools unconditionally (creating their
// config if absent). On --dry-run nothing is written.
func RunConnect(opts ConnectOptions) (results []ToolResult, err error) {
	all := opts.adapters
	if all == nil {
		all = allAdapters()
	}
	byName := map[string]ToolAdapter{}
	for _, a := range all {
		byName[a.Name()] = a
	}

	var selected []ToolAdapter
	if len(opts.Only) > 0 {
		for _, n := range opts.Only {
			a, ok := byName[n]
			if !ok {
				return nil, fmt.Errorf("unknown tool %q (known: %s)", n, strings.Join(adapterNames(all), ", "))
			}
			selected = append(selected, a)
		}
	} else {
		for _, a := range all {
			if a.Detect() {
				selected = append(selected, a)
			}
		}
	}
	excluded := map[string]bool{}
	for _, n := range opts.Exclude {
		if _, ok := byName[n]; !ok {
			return nil, fmt.Errorf("unknown tool %q in --exclude (known: %s)", n, strings.Join(adapterNames(all), ", "))
		}
		excluded[n] = true
	}

	ledger, lerr := LoadConnectLedger(opts.LedgerPath)
	if lerr != nil {
		return nil, lerr
	}
	if !opts.DryRun {
		// Persist whatever we commit — even on a mid-loop error — so a tool whose
		// file was written is always recorded (else its entry looks "foreign" next
		// run). Surface a save error only if the run was otherwise succeeding.
		defer func() {
			if serr := saveConnectLedger(opts.LedgerPath, ledger); serr != nil && err == nil {
				err = serr
			}
		}()
	}

	// Collect per-adapter errors and keep going (isolation, not cascade — one
	// tool's failure must not stop the others being configured). A summary error
	// at the end keeps the process exit code at 1.
	var failures []string
	for _, a := range selected {
		if excluded[a.Name()] {
			continue
		}
		if opts.DryRun {
			results = append(results, ToolResult{Tool: a.Name(), Adapter: a,
				Result: Result{Note: "dry-run: would " + verb(opts.Remove) + " the orbeat-gateway entry"}})
			continue
		}
		_, managed := ledger[a.Name()]
		var (
			r    Result
			aerr error
		)
		if opts.Remove {
			r, aerr = a.RemoveMCP()
			// Drop the ledger entry only on a CONFIRMED removal: the file changed, or
			// the entry was confirmed absent (Note == "" and no error). A parse-failure
			// skip carries a Note — keep the ledger entry so the orbeat entry stays
			// manageable once the config is repaired.
			if aerr == nil && r.Note == "" {
				delete(ledger, a.Name())
			}
		} else {
			r, aerr = a.WriteMCP(opts.GatewayURL, managed)
			if aerr == nil && r.Changed {
				ledger[a.Name()] = LedgerEntry{MCPPath: r.Path}
			}
		}
		if aerr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", a.Name(), aerr))
			results = append(results, ToolResult{Tool: a.Name(), Adapter: a,
				Result: Result{Path: r.Path, Note: "error: " + aerr.Error()}})
			continue
		}
		results = append(results, ToolResult{Tool: a.Name(), Adapter: a, Result: r})
	}
	if len(failures) > 0 {
		return results, fmt.Errorf("connect: %d tool(s) failed: %s", len(failures), strings.Join(failures, "; "))
	}
	return results, nil
}

func adapterNames(all []ToolAdapter) []string {
	names := make([]string, len(all))
	for i, a := range all {
		names[i] = a.Name()
	}
	return names
}

func verb(remove bool) string {
	if remove {
		return "remove"
	}
	return "write"
}
