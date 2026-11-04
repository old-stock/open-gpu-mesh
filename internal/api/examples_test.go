package api

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestExamplesValidate keeps examples/*.yaml in sync with the spec schema.
func TestExamplesValidate(t *testing.T) {
	files, _ := filepath.Glob("../../examples/*.yaml")
	if len(files) == 0 {
		t.Skip("no examples")
	}
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		dec := yaml.NewDecoder(bytes.NewReader([]byte(os.Expand(string(raw), func(string) string { return "x" }))))
		dec.KnownFields(true)
		for {
			var node yaml.Node
			if err := dec.Decode(&node); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			var kind struct{ Kind string }
			_ = node.Decode(&kind)
			if kind.Kind == "queue" || kind.Kind == "schedule" {
				continue
			}
			var spec ContainerGroupSpec
			if err := node.Decode(&spec); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			spec.Normalize()
			if errs := spec.Validate(); errs != nil {
				t.Errorf("%s (%s): %v", filepath.Base(f), spec.Name, errs)
			}
		}
	}
}
