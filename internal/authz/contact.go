package authz

// DefaultContactEmail is the Community edition's default cap-reached contact
// address (docs/specs/2026-08-19-orbeat-community-caps-design.md §6).
// Defined once, here, rather than duplicated in internal/api (which needs
// the identical default for its own server/role cap responses):
// internal/api already imports internal/authz, so the reverse would be an
// import cycle. cmd/api wires ORBEAT_CONTACT_EMAIL to override it in both
// places, via config.Config.ContactEmail; internal/config holds only that
// raw operator override, not the default itself, so internal/config does
// not need to import internal/authz for it.
//
// Kept in its own file, separate from seatcap.go's seat-cap machinery,
// because internal/api's New() (its own cap mechanism) needs this constant
// to compile before any seat-cap code exists.
const DefaultContactEmail = "info@orbeat.org"
