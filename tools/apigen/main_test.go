package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func parse(t *testing.T, raw string) object {
	t.Helper()
	var out object
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

const fullSpec = `{
	"openapi": "3.1.0",
	"info": {"title": "t", "version": "1"},
	"paths": {
		"/a": {
			"get": {"operationId": "getA", "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/A"}}}}}},
			"post": {"operationId": "postA", "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Unused"}}}}}}
		},
		"/b": {"parameters": [], "get": {"operationId": "getB", "responses": {}}}
	},
	"components": {"schemas": {
		"A": {"type": "object", "properties": {"nested": {"$ref": "#/components/schemas/Nested"}}},
		"Nested": {"type": "string"},
		"Unused": {"type": "string"}
	}}
}`

func TestFilterSpecKeepsWantedOperationsAndReachableSchemas(t *testing.T) {
	t.Parallel()

	got, err := filterSpec(parse(t, fullSpec), map[string]string{"getA": ""})
	if err != nil {
		t.Fatalf("filterSpec() error = %v", err)
	}
	if err := checkSnapshot(got, map[string]string{"getA": ""}); err != nil {
		t.Fatalf("checkSnapshot() error = %v", err)
	}
	schemas := got["components"].(object)["schemas"].(object)
	if len(schemas) != 2 || schemas["A"] == nil || schemas["Nested"] == nil {
		t.Fatalf("expected A and Nested, got %v", schemas)
	}
}

func TestFilterSpecRefusesAnUnknownOperation(t *testing.T) {
	t.Parallel()

	_, err := filterSpec(parse(t, fullSpec), map[string]string{"gone": ""})
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("expected an error naming the operation, got %v", err)
	}
}

func TestCheckSnapshotNoticesAnEditedOperationList(t *testing.T) {
	t.Parallel()

	err := checkSnapshot(parse(t, fullSpec), map[string]string{"getA": "", "getB": "", "getC": ""})
	if err == nil || !strings.Contains(err.Error(), "missing getC") || !strings.Contains(err.Error(), "unlisted postA") {
		t.Fatalf("expected missing and unlisted operations, got %v", err)
	}
}

func TestSimplifyRemovesUnionTypes(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, in, want string }{
		{"nullable scalar", `{"anyOf": [{"type": "string"}, {"type": "null"}], "title": "T"}`, `{"type": "string", "nullable": true, "title": "T"}`},
		{"nullable ref", `{"anyOf": [{"$ref": "#/x"}, {"type": "null"}], "title": "T"}`, `{"$ref": "#/x"}`},
		{"ref with siblings", `{"$ref": "#/x", "default": "PYPI"}`, `{"$ref": "#/x"}`},
		{"real union", `{"anyOf": [{"$ref": "#/x"}, {"$ref": "#/y"}], "title": "T"}`, `{"title": "T"}`},
		{"union with null", `{"anyOf": [{"$ref": "#/x"}, {"$ref": "#/y"}, {"type": "null"}]}`, `{}`},
		{"property names", `{"type": "object", "propertyNames": {"$ref": "#/x"}}`, `{"type": "object"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parse(t, tt.in)
			simplify(got)
			if want := parse(t, tt.want); !reflect.DeepEqual(got, want) {
				t.Fatalf("simplify() = %v, want %v", got, want)
			}
		})
	}
}
