package syncclient

import "errors"

// fatalError marks an error that must abort the entire sync: the local managed
// state (the manifest, the sync-root boundary) can no longer be trusted, so
// continuing risks acting on corrupt or hostile state. Errors NOT marked fatal
// are per-unit failures that callers record and skip. Default non-fatal is
// deliberate — a newly added per-unit I/O path continues rather than re-arming
// the abort cascade; a new security boundary is a deliberate act whose author
// marks it fatal as they write it.
type fatalError struct{ err error }

func (e fatalError) Error() string { return e.err.Error() } // message unchanged
func (e fatalError) Unwrap() error { return e.err }

// markFatal wraps err as a whole-sync abort. Returns nil for a nil error. The
// message is unchanged (Error delegates to the inner error), so existing
// error-substring assertions keep working.
func markFatal(err error) error {
	if err == nil {
		return nil
	}
	return fatalError{err}
}

// isFatal reports whether err, or anything it wraps, was marked fatal.
func isFatal(err error) bool {
	var fe fatalError
	return errors.As(err, &fe)
}

// IsFatal is the exported form of isFatal, for callers outside this package
// (e.g. cmd/sync's exit-code mapping) that must distinguish a fatal
// integrity/security abort (exit 2) from a retryable failure (exit 1).
func IsFatal(err error) bool { return isFatal(err) }
