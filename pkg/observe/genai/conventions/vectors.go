package conventions

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// Vector is one golden case: a span's attributes as an SDK of one
// convention sends them, and the attributes the span must have after
// normalisation (the originals and the canonical keys added).
type Vector struct {
	// Name says what the case covers.
	Name string `json:"name"`
	// TraceID is the span's trace id, lowercase hex.
	TraceID string `json:"trace_id"`
	// Input is the span's attributes as sent.
	Input map[string]string `json:"input"`
	// Want is the span's attributes after normalisation.
	Want map[string]string `json:"want"`
}

//go:embed vectors.json
var vectorsJSON []byte

// Vectors returns the golden vectors. Normalize satisfies every one (the
// package's tests prove it); a collector running OTTL's statements must too.
func Vectors() []Vector {
	var v []Vector
	if err := json.Unmarshal(vectorsJSON, &v); err != nil {
		panic(fmt.Sprintf("conventions: vectors.json: %v", err))
	}
	return v
}
