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
// snapshot keeps its zero value, so a server changing a field the command does
// not read cannot break an installed CLI. Use DecodeStrict when losing a field
// would change what the command does.
func (o Operation[T]) Decode(body []byte) (T, error) {
	out, err := o.DecodeStrict(body)
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		err = nil
	}
	return out, err
}

// DecodeStrict parses a response whose fields steer the command, such as a page
// cursor: a read that ended early would look like a complete one.
func (Operation[T]) DecodeStrict(body []byte) (T, error) {
	var out T
	err := json.Unmarshal(body, &out)
	return out, err
}

// Ptr returns a pointer to v, for the generated optional fields.
func Ptr[T any](v T) *T {
	return &v
}
