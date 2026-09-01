package api

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// The three reasons a pin was not honoured, reported as syncArtifactDTO's
// pinOverride. Absent means the pin was served exactly, or that there was no
// pin at all.
//
// They are not a taxonomy of causes so much as an answer to "what do I do
// now": pinOverridePruned is permanent and the developer's only move is to
// pick a surviving revision, pinOverrideFloor is an admin decision somebody
// can be asked about, and pinOverrideAhead means the pin names a revision this
// server has never approved, which on a healthy install means the pins file
// came from somewhere else.
const (
	pinOverrideFloor  = "floor"
	pinOverridePruned = "pruned"
	pinOverrideAhead  = "ahead"
)

// maxPinsPerRequest caps how many ?pin pairs one request may carry. 100 is
// defaultListLimit, the bound this repo already applies to every admin list,
// reused rather than re-chosen so there is one number to raise.
//
// Over the cap is a 400 and never a silent truncation. A truncated pin set is
// the failure shape the whole capability negotiation exists to prevent: the
// server would serve some pins, ignore the rest, and report success, and the
// client would have no way to tell which half it got.
const maxPinsPerRequest = defaultListLimit

// parsePins reads the repeatable ?pin=<artifactId>:<revisionNum> parameter into
// a map from artifact id to the revision that machine asked for.
//
// Every rejection here is a 400, following the ?include precedent v1.22.0 set
// on GET /v1/admin/artifacts: this parameter is introduced by this commit, so
// no client depends on any handling of a bad value yet, and defaulting one
// silently would hand a developer the latest content under the impression she
// pinned it.
//
// A REPEATED ARTIFACT ID IS A 400 rather than last-write-wins. Two pins naming
// one artifact is a request with two answers, and picking either silently is
// the same silent-default failure as the malformed cases above. The pin file
// this parameter exists to carry is keyed on artifactId (spec sec 10.1), so a
// repeat is a client bug rather than a shape any developer can express.
//
// A well-formed pin naming an artifact OUTSIDE the caller's entitled set is
// not rejected and not even noticed here: this function never sees the
// entitled set. It is dropped downstream by simple absence, because the
// artifact is not in the response for a pin to attach to. A revoked
// entitlement leaving a stale local pin is ordinary, and failing the whole
// sync over it would punish a developer for an admin's action; the client
// holds the pin file and can diff it against the returned set, the same
// division POST /v1/sync/deployments already makes for a dropped artifactId.
//
// The caller must only reach this when pinning is supported. On a Community
// server ?pin is ignored without ever being parsed, so none of these 400s can
// fire there (pinning.community.go).
func parsePins(q url.Values) (map[string]int, error) {
	raw := q["pin"]
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxPinsPerRequest {
		return nil, validationError{fmt.Sprintf("at most %d pin parameters per request, got %d", maxPinsPerRequest, len(raw))}
	}
	pins := make(map[string]int, len(raw))
	for _, pair := range raw {
		id, num, ok := strings.Cut(pair, ":")
		if !ok {
			return nil, validationError{`pin must be "<artifactId>:<revisionNum>"`}
		}
		if !uuidRe.MatchString(id) {
			return nil, validationError{"pin artifact id must be a uuid"}
		}
		n, err := strconv.Atoi(num)
		if err != nil || n < 1 {
			// insertRevision numbers from 1, so 0 and every negative name no
			// revision that can exist. Rejecting them here is what keeps
			// pinResolve's own "0 means no pin" sentinel unambiguous.
			return nil, validationError{"pin revision must be an integer >= 1"}
		}
		if _, dup := pins[id]; dup {
			return nil, validationError{"pin names the same artifact more than once"}
		}
		pins[id] = n
	}
	return pins, nil
}

// pinResolution is what the clamp decided for one artifact: which revision to
// serve, the lower bound it was clamped into, and why the pin was not honoured
// if it was not.
type pinResolution struct {
	// Served is the revision whose bytes the caller gets.
	Served int
	// Oldest is max(floor, minNum), spec sec 4.2's `low`: the oldest revision
	// this caller can be served today. It is advertised as oldestServable so
	// `orbeat-sync pin --revision N` can reject a bad number at pin time
	// rather than at the next sync, and it is the number a developer needs in
	// order to act on every one of the three override reasons.
	Oldest int
	// Reason is "" when the pin was served exactly or when there was no pin.
	Reason string
}

// pinResolve is spec sec 4.2's clamp, whole, in one expression:
//
//	low    = max(floor, minNum)
//	served = min(max(pin, low), maxNum)
//
// with an absent pin giving served = maxNum, which is the behaviour every
// release before this one had.
//
// pin <= 0 MEANS ABSENT. That sentinel is safe because insertRevision numbers
// revisions from 1 and parsePins rejects anything below 1, so no real pin can
// collide with it, and it lets the call site pass a map lookup straight in: a
// missing key yields 0 and the whole rule stays one function rather than an
// if-statement at the call site that a later reader would have to find.
//
// THE FLOOR AND THE PRUNE BOUNDARY ARE THE SAME KIND OF THING here, and that
// is why this is written as a clamp rather than as two special cases. Both
// raise the lower bound; neither can lower it. Two special cases would drift
// apart the first time one of them grew a condition.
//
// A PRUNED PIN SERVES THE OLDEST SURVIVOR, NOT THE NEWEST. Serving the newest
// is simpler and is wrong: an unpinned client already receives the newest, so
// a pin degrading to "newest" has degraded to "no pin at all", inverting the
// developer's only stated intent. It also drifts gently, since under a fixed
// ORBEAT_ARTIFACT_REVISION_KEEP minNum advances by one per approval, so a
// degraded pin walks forward a revision at a time instead of jumping to head.
//
// WHEN BOTH BOUNDS BITE, THE BINDING ONE IS REPORTED. Floor 5, minNum 8,
// pin 2 reports pruned, not floor, because lowering the floor would not move
// what this caller is served: revision 2 is gone and revisions 5, 6 and 7 with
// it. Naming the floor there sends an admin to change a control that is not
// what is stopping anybody. Equal bounds report pruned for the same reason,
// since a floor at minNum is not what is withholding a revision below minNum.
//
// maxNum 0 (an artifact with no revision rows, unreachable through any API
// path but reachable by direct SQL) resolves to served 0 == maxNum, so the
// caller serves approved_content and reads no revision at all. That is the
// degenerate case degrading to current behaviour rather than to an error,
// which is the same property sec 5.1 argues for the common path.
func pinResolve(pin, floor, minNum, maxNum int) pinResolution {
	low := max(floor, minNum)
	if pin <= 0 {
		return pinResolution{Served: maxNum, Oldest: low}
	}
	res := pinResolution{Served: min(max(pin, low), maxNum), Oldest: low}
	switch {
	case res.Served == pin:
		// Honoured exactly. No reason, and nothing for the client to report.
	case pin > maxNum:
		res.Reason = pinOverrideAhead
	case floor > minNum:
		res.Reason = pinOverrideFloor
	default:
		res.Reason = pinOverridePruned
	}
	return res
}
