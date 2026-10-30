package api

import "testing"

func TestDecodeKeepsReadableFieldsWhenAnotherFieldChangesType(t *testing.T) {
	t.Parallel()

	run, err := GetJobRun.Decode([]byte(`{"id":"run-1","createTime":17,"status":"BRAND_NEW","triggeredBy":"someone"}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if run.ID != "run-1" || run.Status != "BRAND_NEW" {
		t.Fatalf("expected id and status to survive, got %+v", run)
	}
}

func TestDecodeRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	if _, err := GetJobRun.Decode([]byte(`<html>bad gateway</html>`)); err == nil {
		t.Fatal("expected an error for a non-JSON body")
	}
}
