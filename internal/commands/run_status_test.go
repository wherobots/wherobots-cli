package commands

import (
	"encoding/json"
	"os"
	"testing"

	"wherobots/cli/internal/api"
)

// A status from a regenerated spec fails here until classifyRun handles it,
// rather than hanging or misreporting runs for users.
func TestClassifyRunCoversEveryGeneratedStatus(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../api/openapi.snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Components struct {
			Schemas struct {
				RunStatus struct {
					Enum []api.RunStatus `json:"enum"`
				} `json:"RunStatus"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	statuses := snapshot.Components.Schemas.RunStatus.Enum
	if len(statuses) == 0 {
		t.Fatal("snapshot has no RunStatus values")
	}
	for _, status := range statuses {
		if !status.Valid() {
			t.Errorf("%s is in the snapshot but not in the generated enum: run make generate", status)
		}
		if classifyRun(status) == runPhaseUnknown {
			t.Errorf("classifyRun does not handle %s", status)
		}
	}
}

func TestClassifyRunTreatsAnUnseenStatusAsUnknown(t *testing.T) {
	t.Parallel()

	if got := classifyRun("BRAND_NEW"); got != runPhaseUnknown {
		t.Fatalf("classifyRun(BRAND_NEW) = %v, want unknown", got)
	}
}
