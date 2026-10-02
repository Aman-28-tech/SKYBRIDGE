// Server-side canonical workload validation (FR-004).
//
// The JSON Schema document is authoritative: packages/contracts/schemas/
// canonical-workload.schema.json. Because go:embed cannot reference files
// outside this module, a byte-identical copy lives at schemas/
// canonical-workload.schema.json and a sync test (validate_test.go)
// fails the build if the copy drifts from the canonical file.
package main

import (
	_ "embed"
	"encoding/json"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/canonical-workload.schema.json
var canonicalSchemaBytes []byte

var (
	compiledSchema     *jsonschema.Schema
	compiledSchemaErr  error
	compiledSchemaOnce sync.Once
)

func canonicalSchema() (*jsonschema.Schema, error) {
	compiledSchemaOnce.Do(func() {
		var doc any
		if err := json.Unmarshal(canonicalSchemaBytes, &doc); err != nil {
			compiledSchemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("canonical-workload.schema.json", doc); err != nil {
			compiledSchemaErr = err
			return
		}
		compiledSchema, compiledSchemaErr = c.Compile("canonical-workload.schema.json")
	})
	return compiledSchema, compiledSchemaErr
}

// ValidationFailure is one structured schema violation for the 400 details.
type ValidationFailure struct {
	KeywordLocation  string `json:"keywordLocation"`
	InstanceLocation string `json:"instanceLocation"`
	Error            string `json:"error"`
}

// ValidateCanonical validates spec against the embedded canonical schema.
// A nil return means valid.
func ValidateCanonical(spec any) []ValidationFailure {
	sch, err := canonicalSchema()
	if err != nil {
		return []ValidationFailure{{Error: "schema unavailable: " + err.Error()}}
	}
	if err := sch.Validate(spec); err == nil {
		return nil
	} else if ve, ok := err.(*jsonschema.ValidationError); ok {
		var out []ValidationFailure
		collectFailures(ve.DetailedOutput(), &out)
		if len(out) == 0 {
			out = []ValidationFailure{{Error: ve.Error()}}
		}
		return out
	} else {
		return []ValidationFailure{{Error: err.Error()}}
	}
}

func collectFailures(u *jsonschema.OutputUnit, out *[]ValidationFailure) {
	if u == nil {
		return
	}
	if u.Error != nil {
		*out = append(*out, ValidationFailure{
			KeywordLocation:  u.KeywordLocation,
			InstanceLocation: u.InstanceLocation,
			Error:            u.Error.String(),
		})
	}
	for i := range u.Errors {
		collectFailures(&u.Errors[i], out)
	}
}
