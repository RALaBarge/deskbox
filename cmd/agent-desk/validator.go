package main

import "deskbox/agent-desk/internal/tcs"

// Schema and Validate live in internal/tcs so the daemon's input/output gate
// and the offline tcs-verify linter share one implementation — two schema
// validators drifting apart is exactly the kind of gap this project exists to
// close. These aliases keep the rest of the daemon referring to Schema and
// Validate unqualified, as it always has.
type Schema = tcs.Schema

// Validate returns the contract violations of v against schema (empty = ok).
func Validate(s Schema, v any) []string { return tcs.Validate(s, v) }
