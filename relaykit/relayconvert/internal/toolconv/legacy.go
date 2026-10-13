package toolconv

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
)

type legacyToolIdentity struct{ Namespace, Name, Type string }

// PrepareLegacyResponses resolves aliases before the official extractor can
// discard colliding names. It owns its decoded data; retries never mutate input.
func PrepareLegacyResponses(request any) (*dto.OpenAIResponsesRequest, *convmeta.ResponsesToolState, []types.ConversionDiagnostic, error) {
	var source *dto.OpenAIResponsesRequest
	switch value := request.(type) {
	case *dto.OpenAIResponsesRequest:
		source = value
	case dto.OpenAIResponsesRequest:
		source = &value
	}
	if source == nil {
		return nil, nil, nil, fmt.Errorf("expected Responses request, got %T", request)
	}
	clone := *source
	var tools []map[string]any
	if len(source.Tools) > 0 {
		if err := kitutil.Unmarshal(source.Tools, &tools); err != nil {
			return nil, nil, nil, err
		}
	}
	var items []map[string]any
	var diagnostics []types.ConversionDiagnostic
	if kitutil.GetJsonType(source.Input) == "array" {
		if err := kitutil.Unmarshal(source.Input, &items); err != nil {
			return nil, nil, nil, err
		}
		filtered := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if item["type"] != "additional_tools" {
				filtered = append(filtered, item)
				continue
			}
			raw, err := kitutil.Marshal(item["tools"])
			if err != nil {
				return nil, nil, nil, err
			}
			var additional []map[string]any
			if err = kitutil.Unmarshal(raw, &additional); err != nil {
				return nil, nil, nil, err
			}
			tools = append(tools, additional...)
			diagnostics = append(diagnostics, presentationLoss("input", "additional_tools_flattened", "additional_tools declarations are sent as top-level tools"))
		}
		items = filtered
	}
	flattened := make([]map[string]any, 0, len(tools))
	identities := make([]legacyToolIdentity, 0, len(tools))
	declared := map[legacyToolIdentity]bool{}
	for _, tool := range tools {
		if tool == nil {
			return nil, nil, nil, fmt.Errorf("legacy tool definition cannot be null")
		}
		if tool["type"] == "custom" {
			if nested, ok := tool["custom"].(map[string]any); ok {
				tool = nested
				if tool["type"] == nil {
					tool["type"] = "custom"
				}
			}
		}
		namespace := ""
		members := []map[string]any{tool}
		if tool["type"] == "namespace" {
			namespace = strings.TrimSpace(kitutil.Interface2String(tool["name"]))
			if namespace == "" {
				return nil, nil, nil, fmt.Errorf("legacy namespace requires a name")
			}
			raw, err := kitutil.Marshal(tool["tools"])
			if err != nil {
				return nil, nil, nil, err
			}
			if err = kitutil.Unmarshal(raw, &members); err != nil {
				return nil, nil, nil, fmt.Errorf("namespace %q tools: %w", namespace, err)
			}
		}
		for _, member := range members {
			if member == nil {
				return nil, nil, nil, fmt.Errorf("legacy tool definition cannot be null")
			}
			if member["type"] == nil {
				member["type"] = "function"
			}
			if member["type"] == "function" {
				if nested, ok := member["function"].(map[string]any); ok {
					member = nested
					member["type"] = "function"
				}
			}
			typ := kitutil.Interface2String(member["type"])
			identity := legacyToolIdentity{Namespace: namespace, Name: strings.TrimSpace(kitutil.Interface2String(member["name"])), Type: typ}
			if typ == "function" || typ == "custom" {
				if identity.Name == "" {
					return nil, nil, nil, fmt.Errorf("legacy %s tool requires a name", typ)
				}
				if declared[identity] {
					return nil, nil, nil, fmt.Errorf("duplicate %s tool %q in namespace %q", typ, identity.Name, namespace)
				}
				declared[identity] = true
				strict, err := lowerLegacyStrict(member, "tools.strict")
				if err != nil {
					return nil, nil, nil, err
				}
				if strict {
					diagnostics = append(diagnostics, semanticLoss("tools.strict", "legacy_strict_removed", "legacy Chat profile removes strict execution guarantees; JSON Schema constraints are preserved"))
				}
				// Retain schema keywords, including $id and $defs. Unchanged
				// schemas have no conversion loss, even under strict loss policy.
			}
			if namespace != "" {
				if desc := kitutil.Interface2String(tool["description"]); desc != "" {
					member["description"] = strings.TrimSpace(desc + "\n\n" + kitutil.Interface2String(member["description"]))
				}
			}
			flattened = append(flattened, member)
			identities = append(identities, identity)
		}
	}
	// Resolve unqualified history only when its original name is unambiguous.
	historyIdentities := map[int]legacyToolIdentity{}
	for i, item := range items {
		typ := kitutil.Interface2String(item["type"])
		if typ != "function_call" && typ != "custom_tool_call" {
			continue
		}
		kind := "function"
		if typ == "custom_tool_call" {
			kind = "custom"
		}
		identity := legacyToolIdentity{Namespace: strings.TrimSpace(kitutil.Interface2String(item["namespace"])), Name: strings.TrimSpace(kitutil.Interface2String(item["name"])), Type: kind}
		if identity.Name == "" {
			return nil, nil, nil, fmt.Errorf("input[%d] tool call requires name", i)
		}
		if !declared[identity] && identity.Namespace == "" {
			matches := 0
			var candidate legacyToolIdentity
			for key := range declared {
				if key.Name == identity.Name && key.Type == identity.Type {
					matches++
					candidate = key
				}
			}
			if matches > 1 {
				return nil, nil, nil, fmt.Errorf("input[%d] unqualified tool name %q is ambiguous", i, identity.Name)
			}
			if matches == 1 {
				identity = candidate
			}
		}
		historyIdentities[i] = identity
	}
	all := map[legacyToolIdentity]bool{}
	for key := range declared {
		all[key] = true
	}
	for _, key := range historyIdentities {
		all[key] = true
	}
	aliases := legacyToolAliases(all)
	state := &convmeta.ResponsesToolState{OriginalNames: map[string]convmeta.ResponsesToolIdentity{}, CustomToolNames: map[string]struct{}{}}
	for key, alias := range aliases {
		state.OriginalNames[alias] = convmeta.ResponsesToolIdentity{Namespace: key.Namespace, Name: key.Name}
		if key.Type == "custom" {
			state.CustomToolNames[alias] = struct{}{}
		}
	}
	for i, member := range flattened {
		if alias := aliases[identities[i]]; alias != "" {
			member["name"] = alias
		}
	}
	for i, key := range historyIdentities {
		items[i]["name"] = aliases[key]
		delete(items[i], "namespace")
	}
	var err error
	if len(flattened) > 0 {
		clone.Tools, err = kitutil.Marshal(flattened)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if items != nil {
		clone.Input, err = kitutil.Marshal(items)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if len(source.ToolChoice) > 0 {
		if kitutil.GetJsonType(source.ToolChoice) == "string" {
			var choice string
			if err = kitutil.Unmarshal(source.ToolChoice, &choice); err != nil {
				return nil, nil, nil, err
			}
			if choice == "required" {
				clone.ToolChoice = json.RawMessage(`"auto"`)
				diagnostics = append(diagnostics, semanticLoss("tool_choice", "legacy_choice_auto", "legacy Chat profile lowers required tool_choice to auto"))
			}
		} else {
			var choice map[string]any
			if err = kitutil.Unmarshal(source.ToolChoice, &choice); err != nil {
				return nil, nil, nil, err
			}
			if choice["type"] == "custom" {
				clone.ToolChoice = json.RawMessage(`"auto"`)
				diagnostics = append(diagnostics, semanticLoss("tool_choice", "legacy_choice_auto", "legacy Chat profile lowers custom tool_choice to auto"))
			} else if choice["type"] == "function" {
				name := strings.TrimSpace(kitutil.Interface2String(choice["name"]))
				if name == "" {
					if nested, ok := choice["function"].(map[string]any); ok {
						name = kitutil.Interface2String(nested["name"])
					}
				}
				key := legacyToolIdentity{Namespace: strings.TrimSpace(kitutil.Interface2String(choice["namespace"])), Name: name, Type: "function"}
				if !declared[key] && key.Namespace == "" {
					matches := 0
					for candidate := range declared {
						if candidate.Name == name && candidate.Type == "function" {
							matches++
							key = candidate
						}
					}
					if matches != 1 {
						return nil, nil, nil, fmt.Errorf("tool_choice function %q is unknown or ambiguous", name)
					}
				}
				if !declared[key] {
					return nil, nil, nil, fmt.Errorf("tool_choice function %q is not declared", name)
				}
				choice["name"] = aliases[key]
				delete(choice, "namespace")
				delete(choice, "function")
				clone.ToolChoice, err = kitutil.Marshal(choice)
				if err != nil {
					return nil, nil, nil, err
				}
			}
		}
	}
	if len(source.Text) > 0 && kitutil.GetJsonType(source.Text) == "object" {
		var text map[string]any
		if err := kitutil.Unmarshal(source.Text, &text); err != nil {
			return nil, nil, nil, err
		}
		if format, ok := text["format"].(map[string]any); ok {
			strict, strictErr := lowerLegacyStrict(format, "text.format.strict")
			if strictErr != nil {
				return nil, nil, nil, strictErr
			}
			if strict {
				diagnostics = append(diagnostics, semanticLoss("text.format.strict", "legacy_strict_removed", "legacy Chat profile removes strict output guarantees; the output schema is preserved"))
			}
			clone.Text, err = kitutil.Marshal(text)
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}
	return &clone, state, diagnostics, nil
}

// legacyToolAliases preserves valid unique names and hashes every colliding or
// invalid identity deterministically. Sorting also makes collision resolution
// independent of declaration order. Every result meets Chat's 64-byte alphabet.
func legacyToolAliases(keys map[legacyToolIdentity]bool) map[legacyToolIdentity]string {
	ordered := make([]legacyToolIdentity, 0, len(keys))
	counts := map[string]int{}
	for key := range keys {
		ordered = append(ordered, key)
		counts[convmeta.NamespacedToolName(key.Namespace, key.Name)]++
	}
	slices.SortFunc(ordered, func(a, b legacyToolIdentity) int {
		return strings.Compare(a.Namespace+"\x00"+a.Name+"\x00"+a.Type, b.Namespace+"\x00"+b.Name+"\x00"+b.Type)
	})
	result := map[legacyToolIdentity]string{}
	used := map[string]bool{}
	for _, key := range ordered {
		name := convmeta.NamespacedToolName(key.Namespace, key.Name)
		valid := len(name) > 0 && len(name) <= 64
		for _, r := range name {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				valid = false
			}
		}
		if valid && counts[name] == 1 {
			result[key] = name
			used[name] = true
		}
	}
	for _, key := range ordered {
		if result[key] != "" {
			continue
		}
		base := convmeta.NamespacedToolName(key.Namespace, key.Name)
		base = strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
				return r
			}
			return '_'
		}, base)
		if len(base) > 40 {
			base = base[:40]
		}
		if base == "" {
			base = "tool"
		}
		for salt := 0; ; salt++ {
			sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", key.Namespace, key.Name, key.Type, salt)))
			alias := fmt.Sprintf("%s_%x", base, sum[:10])
			if !used[alias] {
				result[key] = alias
				used[alias] = true
				break
			}
		}
	}
	return result
}

// lowerLegacyStrict removes the unsupported flag, reporting an actual loss
// only when strict guarantees were requested. Null and false impose none.
func lowerLegacyStrict(value map[string]any, path string) (bool, error) {
	strict, exists := value["strict"]
	if !exists {
		return false, nil
	}
	delete(value, "strict")
	if strict == nil {
		return false, nil
	}
	enabled, ok := strict.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean or null", path)
	}
	return enabled, nil
}
