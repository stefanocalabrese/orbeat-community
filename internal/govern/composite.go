package govern

import "context"

// CompositeScanner runs a sequence of scanners and concatenates their findings.
// It never returns an error itself: the rule scanner cannot error, and the LLM
// scanner fails open internally (returns an info finding with a nil error). A
// scanner that does return an error is treated as fatal and propagated, but no
// built-in scanner does.
type CompositeScanner struct {
	scanners []Scanner
}

// NewCompositeScanner composes scanners in order. nil entries are skipped, so
// callers can pass an optional scanner without a guard.
func NewCompositeScanner(scanners ...Scanner) Scanner {
	return &CompositeScanner{scanners: scanners}
}

func (c *CompositeScanner) Scan(ctx context.Context, p ArtifactPayload) ([]Finding, error) {
	var all []Finding
	for _, sc := range c.scanners {
		if sc == nil {
			continue
		}
		f, err := sc.Scan(ctx, p)
		if err != nil {
			return nil, err
		}
		all = append(all, f...)
	}
	return all, nil
}
