// Package values is the add-on values seam: it produces the Helm values document shown in the
// portal's in-browser editor, seeded with the chart's full values.yaml merged with the platform's
// curated catalog overrides. A user may then save a full-document override per cluster (see
// domain.Addon.ValuesOverride), which the reconciler installs with `helm ... -f <override>`.
//
// The Provider seam fetches the chart's own defaults: the real implementation shells to
// `helm show values`, the fake synthesizes a plausible doc from the catalog. Merged overlays the
// catalog's `--set` overrides onto those defaults - the same overrides the helm manager would apply
// - so the editor's starting point is exactly what an un-customized install produces.
package values

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Daniel-Vaz/KaaS-demo/internal/catalog"
	"gopkg.in/yaml.v3"
)

// Merged overlays the catalog entry's `--set` overrides onto the chart's default values (chartYAML)
// and re-encodes the result as YAML. It is the editor's seed: the effective values an install with
// no per-cluster override would apply. chartYAML may be empty (the chart's built-in defaults are
// then unknown and only the catalog overrides appear).
//
// The merge is done on the YAML node tree, not a decoded map, so the chart's own comments and key
// ordering survive into the editor - the user sees the full annotated values.yaml, not a stripped
// dump. Output is encoded at Helm's conventional 2-space indent.
func Merged(entry catalog.Addon, chartYAML string) (string, error) {
	var doc yaml.Node
	if strings.TrimSpace(chartYAML) != "" {
		if err := yaml.Unmarshal([]byte(chartYAML), &doc); err != nil {
			return "", fmt.Errorf("values: parse chart defaults for %q: %w", entry.Name, err)
		}
	}
	// Locate (or create) the document's root mapping node.
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		root = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Content[0] = root
	}

	// Apply catalog overrides in sorted key order for determinism.
	keys := make([]string, 0, len(entry.Values))
	for k := range entry.Values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		toks, err := parsePath(k)
		if err != nil {
			return "", fmt.Errorf("values: apply override %q on %q: %w", k, entry.Name, err)
		}
		if err := setNode(root, toks, coerce(entry.Values[k])); err != nil {
			return "", fmt.Errorf("values: apply override %q on %q: %w", k, entry.Name, err)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", fmt.Errorf("values: encode merged values for %q: %w", entry.Name, err)
	}
	_ = enc.Close()
	return buf.String(), nil
}

// setNode applies one Helm --set path onto a YAML node tree, creating mapping/sequence nodes as
// needed and leaving untouched siblings (and their comments) intact.
func setNode(container *yaml.Node, toks []token, value any) error {
	if len(toks) == 0 {
		return nil
	}
	t := toks[0]
	last := len(toks) == 1
	if t.index < 0 { // map key
		if container.Kind != yaml.MappingNode {
			*container = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		}
		for i := 0; i+1 < len(container.Content); i += 2 {
			if container.Content[i].Value == t.key {
				if last {
					*container.Content[i+1] = *valueNode(value)
					return nil
				}
				return setNode(container.Content[i+1], toks[1:], value)
			}
		}
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: t.key}
		child := &yaml.Node{}
		container.Content = append(container.Content, key, child)
		if last {
			*child = *valueNode(value)
			return nil
		}
		return setNode(child, toks[1:], value)
	}
	// list index
	if container.Kind != yaml.SequenceNode {
		*container = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	}
	for len(container.Content) <= t.index {
		container.Content = append(container.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"})
	}
	if last {
		*container.Content[t.index] = *valueNode(value)
		return nil
	}
	return setNode(container.Content[t.index], toks[1:], value)
}

// valueNode encodes a Go scalar into a YAML node with the tag yaml.v3 infers (bool/int/string).
func valueNode(v any) *yaml.Node {
	n := &yaml.Node{}
	_ = n.Encode(v)
	return n
}

// Valid reports whether s is a parseable YAML document (used to validate a user's edited override
// before it is stored). An empty document is valid (it means "reset to catalog defaults").
func Valid(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var v any
	if err := yaml.Unmarshal([]byte(s), &v); err != nil {
		return fmt.Errorf("values: invalid YAML: %w", err)
	}
	return nil
}

// token is one step of a Helm --set path: a map key (index < 0) or a list index (key == "").
type token struct {
	key   string
	index int
}

// parsePath splits a Helm --set key into tokens: dots separate map keys; a "[n]" suffix is a list
// index. E.g. "a.b[0].c" -> [key a][key b][index 0][key c].
func parsePath(key string) ([]token, error) {
	var toks []token
	for _, part := range strings.Split(key, ".") {
		name := part
		if i := strings.IndexByte(part, '['); i >= 0 {
			name = part[:i]
			rest := part[i:]
			if name != "" {
				toks = append(toks, token{key: name, index: -1})
			}
			for rest != "" {
				if rest[0] != '[' {
					return nil, fmt.Errorf("bad index syntax in %q", key)
				}
				j := strings.IndexByte(rest, ']')
				if j < 0 {
					return nil, fmt.Errorf("unterminated index in %q", key)
				}
				n, err := strconv.Atoi(rest[1:j])
				if err != nil || n < 0 {
					return nil, fmt.Errorf("bad index %q in %q", rest[1:j], key)
				}
				toks = append(toks, token{index: n})
				rest = rest[j+1:]
			}
			continue
		}
		toks = append(toks, token{key: name, index: -1})
	}
	return toks, nil
}

// coerce turns a catalog override string into the typed value Helm's --set would infer: booleans and
// integers become their scalar types; everything else stays a string.
func coerce(s string) any {
	switch s {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return s
}
