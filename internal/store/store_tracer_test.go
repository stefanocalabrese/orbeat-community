package store

import (
	"context"
	"testing"
)

func TestNewWithTracerNilWorks(t *testing.T) {
	ctx := context.Background()
	st, err := NewWithTracer(ctx, testDSN, nil)
	if err != nil {
		t.Fatalf("NewWithTracer nil: %v", err)
	}
	defer st.Close()
	if st.PoolStat() == nil {
		t.Fatalf("PoolStat should be non-nil for a top-level store")
	}
	// a real query must still work through the ParseConfig/NewWithConfig path
	var n int
	if err := st.pool.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("query: %v n=%d", err, n)
	}
}
