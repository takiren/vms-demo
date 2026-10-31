// Package api wires the HTTP API: the generated OpenAPI server, request
// validation, error handling and the handler implementations.
package api

//go:generate go tool oapi-codegen -config gen/oapi-codegen.yaml -o gen/api.gen.go ../../../api/openapi.yaml
