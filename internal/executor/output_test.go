package executor

import (
	"bytes"
	"testing"
)

func TestWriteSuccessResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    []byte
		want    string
		wantErr string
	}{
		{name: "nil body writes nothing", body: nil},
		{name: "empty body writes nothing", body: []byte{}},
		{name: "whitespace body writes nothing", body: []byte(" \r\n\t")},
		{name: "JSON body is written verbatim", body: []byte(`{"id": "t-1"}`), want: `{"id": "t-1"}`},
		{name: "invalid JSON body fails", body: []byte("<html>"), wantErr: "response body is not valid JSON"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			err := WriteSuccessResponse(&out, tt.body)

			if tt.wantErr == "" && err != nil {
				t.Fatalf("WriteSuccessResponse() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr) {
				t.Fatalf("WriteSuccessResponse() error = %v, want %q", err, tt.wantErr)
			}
			if got := out.String(); got != tt.want {
				t.Fatalf("stdout = %q, want %q", got, tt.want)
			}
		})
	}
}
