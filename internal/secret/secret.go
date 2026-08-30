// Package secret holds credentials that must never appear in logs.
package secret

// String is a credential. fmt and slog print it as "****".
type String string

func (s String) String() string   { return "****" }
func (s String) GoString() string { return "secret.String(****)" }
func (s String) Unwrap() string   { return string(s) }
func (s String) Empty() bool      { return s == "" }
