// Package api holds the Wherobots API types the hand-written commands use.
// api.gen.go is generated from openapi.snapshot.json by `make generate`.
package api

import (
	"encoding/json"
	"errors"
)

// Operation names an endpoint and the type its success response decodes to.
type Operation[T any] struct {
	Method string
	Path   string
}

// Decode parses a response body. A field whose JSON type no longer matches the
// snapshot keeps its zero value, so a new server changing a field this command
// does not read cannot break an installed CLI.
func (Operation[T]) Decode(body []byte) (T, error) {
	var out T
	err := json.Unmarshal(body, &out)
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		err = nil
	}
	return out, err
}

// Ptr returns a pointer to v, for the generated optional fields.
func Ptr[T any](v T) *T {
	return &v
}
