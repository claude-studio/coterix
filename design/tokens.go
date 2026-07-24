// Package design exposes the versioned design assets used by the application.
package design

import _ "embed"

//go:embed coterix-color-tokens.json
var coterixColorTokensJSON string

// CoterixColorTokensJSON returns the embedded, immutable color-token source.
func CoterixColorTokensJSON() string {
	return coterixColorTokensJSON
}
