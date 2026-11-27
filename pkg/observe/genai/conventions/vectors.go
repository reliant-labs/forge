package conventions

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/pkg/observe/genai"
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

// Vectors returns the legacy string-only golden vectors.
// Deprecated: use TypedVectors for OTLP attribute values.
func Vectors() []Vector {
	var v []Vector
	if err := json.Unmarshal(vectorsJSON, &v); err != nil {
		panic(fmt.Sprintf("conventions: vectors.json: %v", err))
	}
	return v
}

// TypedVector is a collector test case using OTLP value types: token counts
// are int64, costs float64, and names and enum values strings.
type TypedVector struct {
	Name    string
	TraceID string
	Input   map[string]any
	Want    map[string]any
}

// TypedVectors returns the OTTL golden vectors with their OTLP value types.
// Consumers must insert these values without stringifying them and assert both
// the resulting values and their types after executing OTTL().
func TypedVectors() []TypedVector {
	var out []TypedVector
	for _, v := range Vectors() {
		out = append(out, TypedVector{
			Name: v.Name, TraceID: v.TraceID,
			Input: typedAttributes(v.Input), Want: typedAttributes(v.Want),
		})
	}
	return out
}

func typedAttributes(attrs map[string]string) map[string]any {
	out := make(map[string]any, len(attrs))
	for k, v := range attrs {
		switch {
		case strings.HasPrefix(k, "gen_ai.usage.") || strings.HasPrefix(k, "llm.token_count.") || k == tlReasoning:
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				panic(fmt.Sprintf("conventions: token vector %s: %v", k, err))
			}
			out[k] = n
		case k == oiCostTotal || k == genai.AttrCostUSD:
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				panic(fmt.Sprintf("conventions: cost vector %s: %v", k, err))
			}
			out[k] = n
		default:
			out[k] = v
		}
	}
	return out
}
