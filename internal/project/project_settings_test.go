package project

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestProjectSettings_ModelRoundTrip verifies that the Model field in
// ProjectSettings marshals and unmarshals correctly, and that an empty
// Model is omitted from JSON output (omitempty).
func TestProjectSettings_ModelRoundTrip(t *testing.T) {
	t.Run("model_persists", func(t *testing.T) {
		s := ProjectSettings{Model: "opus"}
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !strings.Contains(string(data), `"model":"opus"`) {
			t.Errorf("marshaled JSON missing model field: %s", data)
		}
		var got ProjectSettings
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.Model != "opus" {
			t.Errorf("round-trip: got %q, want %q", got.Model, "opus")
		}
	})

	t.Run("empty_model_omitted", func(t *testing.T) {
		s := ProjectSettings{Model: ""}
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if strings.Contains(string(data), `"model"`) {
			t.Errorf("empty model should be omitted from JSON, got: %s", data)
		}
	})
}
