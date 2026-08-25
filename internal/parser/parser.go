package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"serverless-to-ecs/internal/model"
)

// RawTemplate is the generic CFN template structure after format normalization.
// All YAML intrinsic functions (!Ref, !GetAtt, etc.) are converted to their
// JSON equivalents ({"Ref": ...}, {"Fn::GetAtt": ...}) so downstream code
// only handles one representation.
type RawTemplate struct {
	AWSTemplateFormatVersion string                    `json:"AWSTemplateFormatVersion" yaml:"AWSTemplateFormatVersion"`
	Transform                interface{}               `json:"Transform" yaml:"Transform"`
	Description              string                    `json:"Description" yaml:"Description"`
	Globals                  map[string]interface{}     `json:"Globals" yaml:"Globals"`
	Resources                map[string]RawResource    `json:"Resources" yaml:"Resources"`
	Outputs                  map[string]interface{}    `json:"Outputs" yaml:"Outputs"`
}

// RawResource is a single CFN resource before type-specific extraction.
type RawResource struct {
	Type       string                 `json:"Type" yaml:"Type"`
	Properties map[string]interface{} `json:"Properties" yaml:"Properties"`
	DependsOn  interface{}            `json:"DependsOn" yaml:"DependsOn"`
	Metadata   map[string]interface{} `json:"Metadata" yaml:"Metadata"`
}

// ParseFile reads a CFN/SAM template from disk and returns the complete resource graph.
func ParseFile(path string) (*model.Graph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}

	raw, err := loadRaw(data)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}

	// Apply SAM Globals defaults before extraction. This mutates resource
	// Properties in place so extractors see the merged values.
	applyGlobals(raw)

	g := model.NewGraph()
	g.Description = raw.Description
	g.TemplateVersion = raw.AWSTemplateFormatVersion
	g.IsSAM = isSAMTemplate(raw.Transform)

	// Iterate resources in a fixed (sorted) order rather than raw.Resources'
	// map order, which Go randomizes per run. Edge/route creation appends to
	// slices in iteration order, so an unsorted iteration would make the
	// generated Terraform (service ordering, ALB listener priorities, etc.)
	// non-reproducible between runs of the same input template.
	logicalIDs := make([]string, 0, len(raw.Resources))
	for logicalID := range raw.Resources {
		logicalIDs = append(logicalIDs, logicalID)
	}
	sort.Strings(logicalIDs)

	// First pass: extract typed resources.
	for _, logicalID := range logicalIDs {
		extractResource(g, logicalID, raw.Resources[logicalID])
	}

	// Second pass: resolve inter-resource references and build edges.
	for _, logicalID := range logicalIDs {
		resolveEdges(g, logicalID, raw.Resources[logicalID])
	}

	// Third pass: SAM inline events (these define edges implicitly).
	if g.IsSAM {
		for _, logicalID := range logicalIDs {
			extractSAMEvents(g, logicalID, raw.Resources[logicalID])
		}
	}

	return g, nil
}

// loadRaw detects format (JSON vs YAML) and unmarshals into RawTemplate.
// For YAML, intrinsic function tags are normalized to JSON-form maps.
func loadRaw(data []byte) (*RawTemplate, error) {
	trimmed := strings.TrimSpace(string(data))

	// JSON detection: starts with '{'.
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var raw RawTemplate
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("json: %w", err)
		}
		return &raw, nil
	}

	// YAML path: use yaml.Node tree to preserve intrinsic function tags,
	// then convert to a generic map with JSON-form intrinsics.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}

	// yaml.Unmarshal wraps everything in a Document node.
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("yaml: expected document node at root")
	}

	normalized := nodeToInterface(root.Content[0])
	// Re-serialize to JSON, then unmarshal into RawTemplate.
	// This round-trip is intentional: it lets us reuse the JSON struct tags
	// and avoids writing a second set of manual map-walking code.
	jsonBytes, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("normalize: %w", err)
	}

	var raw RawTemplate
	if err := json.Unmarshal(jsonBytes, &raw); err != nil {
		return nil, fmt.Errorf("normalized json: %w", err)
	}
	return &raw, nil
}

// nodeToInterface recursively converts a yaml.Node tree into Go primitives,
// translating YAML-shorthand intrinsic functions to their JSON equivalents.
//
//	!Ref X            → {"Ref": "X"}
//	!GetAtt X.Y       → {"Fn::GetAtt": ["X", "Y"]}
//	!Sub "..."        → {"Fn::Sub": "..."}
//	!Select [i, list] → {"Fn::Select": [i, list]}
//	!Join [sep, list] → {"Fn::Join": [sep, list]}
//	!If [cond, t, f]  → {"Fn::If": [cond, t, f]}
//	!ImportValue X     → {"Fn::ImportValue": "X"}
//	!Split [d, s]      → {"Fn::Split": [d, s]}
func nodeToInterface(n *yaml.Node) interface{} {
	// Handle intrinsic function tags.
	if fn, ok := intrinsicTag(n); ok {
		return fn
	}

	switch n.Kind {
	case yaml.ScalarNode:
		return scalarValue(n)

	case yaml.SequenceNode:
		seq := make([]interface{}, 0, len(n.Content))
		for _, child := range n.Content {
			seq = append(seq, nodeToInterface(child))
		}
		return seq

	case yaml.MappingNode:
		m := make(map[string]interface{}, len(n.Content)/2)
		for i := 0; i < len(n.Content)-1; i += 2 {
			key := n.Content[i].Value
			val := nodeToInterface(n.Content[i+1])
			m[key] = val
		}
		return m

	case yaml.AliasNode:
		if n.Alias != nil {
			return nodeToInterface(n.Alias)
		}
		return nil

	default:
		return nil
	}
}

// intrinsicTag checks if a node has a CFN intrinsic function tag and returns
// the JSON-form equivalent.
func intrinsicTag(n *yaml.Node) (map[string]interface{}, bool) {
	switch n.Tag {
	case "!Ref":
		return map[string]interface{}{"Ref": scalarOrChildren(n)}, true

	case "!GetAtt":
		// !GetAtt can be "Resource.Attribute" (scalar) or [Resource, Attribute] (sequence).
		if n.Kind == yaml.ScalarNode {
			parts := strings.SplitN(n.Value, ".", 2)
			if len(parts) == 2 {
				return map[string]interface{}{
					"Fn::GetAtt": []interface{}{parts[0], parts[1]},
				}, true
			}
			return map[string]interface{}{
				"Fn::GetAtt": []interface{}{n.Value, ""},
			}, true
		}
		return map[string]interface{}{
			"Fn::GetAtt": childrenToSlice(n),
		}, true

	case "!Sub":
		return map[string]interface{}{"Fn::Sub": scalarOrChildren(n)}, true

	case "!Join":
		return map[string]interface{}{"Fn::Join": childrenToSlice(n)}, true

	case "!Select":
		return map[string]interface{}{"Fn::Select": childrenToSlice(n)}, true

	case "!If":
		return map[string]interface{}{"Fn::If": childrenToSlice(n)}, true

	case "!ImportValue":
		return map[string]interface{}{"Fn::ImportValue": scalarOrChildren(n)}, true

	case "!Split":
		return map[string]interface{}{"Fn::Split": childrenToSlice(n)}, true

	case "!FindInMap":
		return map[string]interface{}{"Fn::FindInMap": childrenToSlice(n)}, true

	case "!Base64":
		return map[string]interface{}{"Fn::Base64": scalarOrChildren(n)}, true

	case "!Condition":
		return map[string]interface{}{"Condition": scalarOrChildren(n)}, true
	}

	return nil, false
}

// scalarOrChildren returns the scalar value if the node is scalar,
// otherwise recursively processes children.
func scalarOrChildren(n *yaml.Node) interface{} {
	if n.Kind == yaml.ScalarNode {
		return scalarValue(n)
	}
	if n.Kind == yaml.SequenceNode {
		return childrenToSlice(n)
	}
	if n.Kind == yaml.MappingNode {
		return nodeToInterface(n)
	}
	return n.Value
}

func childrenToSlice(n *yaml.Node) []interface{} {
	if n.Kind != yaml.SequenceNode {
		return []interface{}{scalarOrChildren(n)}
	}
	out := make([]interface{}, 0, len(n.Content))
	for _, child := range n.Content {
		out = append(out, nodeToInterface(child))
	}
	return out
}

// scalarValue converts a YAML scalar node to an appropriate Go type.
func scalarValue(n *yaml.Node) interface{} {
	if n.Kind != yaml.ScalarNode {
		return n.Value
	}
	switch n.Tag {
	case "!!bool":
		return n.Value == "true"
	case "!!int":
		var i int
		if _, err := fmt.Sscanf(n.Value, "%d", &i); err == nil {
			return i
		}
		return n.Value
	case "!!float":
		var f float64
		if _, err := fmt.Sscanf(n.Value, "%g", &f); err == nil {
			return f
		}
		return n.Value
	default:
		return n.Value
	}
}

// samGlobalsMapping maps SAM Globals section keys to CFN resource types.
var samGlobalsMapping = map[string]string{
	"Function":    "AWS::Serverless::Function",
	"Api":         "AWS::Serverless::Api",
	"HttpApi":     "AWS::Serverless::HttpApi",
	"SimpleTable": "AWS::Serverless::SimpleTable",
}

// Properties where SAM Globals performs a shallow map merge rather than
// full replacement. Per SAM spec, Environment (specifically its Variables
// sub-key) is merged: resource-level vars override same-key globals, but
// global-only keys are preserved.
var samMergeProperties = map[string]bool{
	"Environment": true,
	"VpcConfig":   true,
}

// applyGlobals merges SAM Globals defaults into each matching resource's
// Properties. Mutates raw.Resources in place.
//
// Merge rules (matching SAM transform behavior):
//   - Scalar/list properties: resource value wins if present; otherwise global applies.
//   - Map properties in samMergeProperties: one-level-deep merge. Sub-maps are
//     themselves merged (resource keys win); scalars within the map follow the
//     same "resource wins" rule.
func applyGlobals(raw *RawTemplate) {
	if len(raw.Globals) == 0 {
		return
	}

	for globalsKey, resType := range samGlobalsMapping {
		globalProps, ok := raw.Globals[globalsKey].(map[string]interface{})
		if !ok || len(globalProps) == 0 {
			continue
		}

		for logicalID, res := range raw.Resources {
			if res.Type != resType {
				continue
			}
			if res.Properties == nil {
				res.Properties = make(map[string]interface{})
			}
			mergeGlobals(res.Properties, globalProps)
			raw.Resources[logicalID] = res
		}
	}
}

// mergeGlobals applies global defaults into dst. dst values take precedence.
func mergeGlobals(dst, globals map[string]interface{}) {
	for key, globalVal := range globals {
		existing, exists := dst[key]
		if !exists {
			// Resource doesn't set this property — use global.
			dst[key] = globalVal
			continue
		}

		// For merge-eligible map properties, do a one-level-deep merge.
		if samMergeProperties[key] {
			dstMap, dstOK := existing.(map[string]interface{})
			globalMap, globalOK := globalVal.(map[string]interface{})
			if dstOK && globalOK {
				mergeMapShallow(dstMap, globalMap)
			}
			// If either side isn't a map, resource value wins (type mismatch).
		}
		// For scalars and lists, resource value already wins — nothing to do.
	}
}

// mergeMapShallow copies keys from src into dst where dst doesn't already
// have them. For sub-map values (e.g. Environment.Variables), it recurses
// one more level.
func mergeMapShallow(dst, src map[string]interface{}) {
	for key, srcVal := range src {
		existing, exists := dst[key]
		if !exists {
			dst[key] = srcVal
			continue
		}
		// One more level: merge sub-maps (e.g. Variables within Environment).
		dstSub, dstOK := existing.(map[string]interface{})
		srcSub, srcOK := srcVal.(map[string]interface{})
		if dstOK && srcOK {
			for k, v := range srcSub {
				if _, has := dstSub[k]; !has {
					dstSub[k] = v
				}
			}
		}
	}
}

// isSAMTemplate checks if the template uses the SAM transform.
func isSAMTemplate(transform interface{}) bool {
	switch v := transform.(type) {
	case string:
		return strings.Contains(v, "Serverless")
	case []interface{}:
		for _, t := range v {
			if s, ok := t.(string); ok && strings.Contains(s, "Serverless") {
				return true
			}
		}
	}
	return false
}
