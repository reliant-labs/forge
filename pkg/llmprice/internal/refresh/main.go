// Command refresh rewrites pkg/llmprice/litellm_prices.json from LiteLLM's
// model price map at a release tag:
//
//	go run ./pkg/llmprice/internal/refresh -tag v1.104.0
//	go run ./pkg/llmprice/internal/refresh -tag v1.104.0 -in model_prices_and_context_window.json
//
// Use the tag of the LiteLLM image the hosted LLM gateway runs
// (control-plane deploy/kcl/lib/infra.k), so an estimate equals what the
// gateway would bill. It keeps the token-priced entries (modes chat,
// completion, responses, embedding) and their four base per-token rates,
// nothing else, one entry per line in key order so a refresh diffs by model.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The modes whose entries price tokens. Image, audio, video, rerank and the
// rest price other units.
var tokenModes = map[string]bool{"chat": true, "completion": true, "responses": true, "embedding": true}

type entry struct {
	Provider   string   `json:"provider"`
	Mode       string   `json:"mode"`
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

func main() {
	tag := flag.String("tag", "", "LiteLLM release tag, e.g. v1.104.0 (required)")
	in := flag.String("in", "", "read the price map from this file instead of downloading the tag's")
	dst := flag.String("out", filepath.Join("pkg", "llmprice", "litellm_prices.json"), "the file to write (run from the repository root)")
	flag.Parse()
	if *tag == "" {
		fail(fmt.Errorf("-tag is required"))
	}
	raw, err := read(*tag, *in)
	if err != nil {
		fail(err)
	}
	out, n, err := trim(raw, *tag)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(*dst, out, 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %s: %d models from LiteLLM %s\n", *dst, n, *tag)
}

func read(tag, in string) ([]byte, error) {
	if in != "" {
		return os.ReadFile(in)
	}
	url := "https://raw.githubusercontent.com/BerriAI/litellm/" + tag + "/model_prices_and_context_window.json"
	c := &http.Client{Timeout: 2 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func trim(raw []byte, tag string) ([]byte, int, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, 0, err
	}
	kept := map[string]entry{}
	for key, msg := range all {
		var v struct {
			Provider   string   `json:"litellm_provider"`
			Mode       string   `json:"mode"`
			Input      *float64 `json:"input_cost_per_token"`
			Output     *float64 `json:"output_cost_per_token"`
			CacheRead  *float64 `json:"cache_read_input_token_cost"`
			CacheWrite *float64 `json:"cache_creation_input_token_cost"`
		}
		// sample_spec documents the format with string values: skipped
		// with every other entry that does not decode.
		if json.Unmarshal(msg, &v) != nil || !tokenModes[v.Mode] || v.Input == nil {
			continue
		}
		kept[key] = entry{Provider: v.Provider, Mode: v.Mode, Input: v.Input, Output: v.Output, CacheRead: v.CacheRead, CacheWrite: v.CacheWrite}
	}
	keys := make([]string, 0, len(kept))
	for k := range kept {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	var b bytes.Buffer
	fmt.Fprintf(&b, "{\n  \"source\": \"litellm\",\n  \"version\": %q,\n  \"license\": \"MIT, Copyright (c) 2023 Berri AI (LITELLM_LICENSE)\",\n  \"models\": {\n", tag)
	for i, k := range keys {
		kb, _ := json.Marshal(k)
		vb, err := json.Marshal(kept[k])
		if err != nil {
			return nil, 0, err
		}
		sep := ","
		if i == len(keys)-1 {
			sep = ""
		}
		fmt.Fprintf(&b, "    %s: %s%s\n", kb, vb, sep)
	}
	b.WriteString("  }\n}\n")
	if strings.Count(b.String(), "\n") < 10 {
		return nil, 0, fmt.Errorf("kept no models: is %s a LiteLLM price map?", tag)
	}
	return b.Bytes(), len(keys), nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "refresh:", err)
	os.Exit(1)
}
