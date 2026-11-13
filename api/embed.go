// Package apispec embeds the OpenAPI 3.1 document and the docs page.
package apispec

import _ "embed"

//go:embed openapi.json
var OpenAPI []byte

//go:embed docs.html
var DocsHTML []byte
