// Package api embeds the OpenAPI specification so the running service can serve
// the exact contract it was built from.
//
// Serving the spec from the binary rather than from a sidecar or a wiki removes
// the most common way for documentation to drift: there is no separate artefact
// to forget to update, and the contract test in tests/openapi_contract_test.go
// compares this file against the routes the router actually registers.
package api

import _ "embed"

// OpenAPISpec is the contents of api/openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
