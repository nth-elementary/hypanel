// Package panel holds the module's root files. The README is embedded so the panel can use
// it as the starting text of its editable Guide.
package panel

import _ "embed"

//go:embed README.md
var Readme string
