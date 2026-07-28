package manager

// Helpers for reading the untyped Config and HostConfig maps that
// docker.ContainerInspect keeps verbatim. They all tolerate missing keys and
// unexpected types, because the point of holding that JSON untyped is to survive
// engine versions that changed a field we never look at.

// mapString returns m[key] if it is a string, otherwise "".
func mapString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// mapBool returns m[key] if it is a bool, otherwise false.
func mapBool(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	b, _ := m[key].(bool)
	return b
}

// mapSub returns m[key] if it is a nested object, otherwise nil.
func mapSub(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	sub, _ := m[key].(map[string]any)
	return sub
}

// mapStrings returns m[key] as a string slice. JSON arrays decode to []any, so
// each element is converted individually and non-strings are skipped.
func mapStrings(m map[string]any, key string) []string {
	if m == nil {
		return nil
	}
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// mapLabels returns m["Labels"] as a string map.
func mapLabels(m map[string]any) map[string]string {
	raw := mapSub(m, "Labels")
	if raw == nil {
		return nil
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		if s, ok := value.(string); ok {
			out[key] = s
		}
	}
	return out
}

// cloneMap makes a shallow copy of m.
//
// Shallow is deliberate: recreate rewrites only top-level keys, and the nested
// values are handed straight back to Docker as they came out of inspect. Nothing
// mutates them in place, so there is nothing to deep-copy.
func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for key, value := range m {
		out[key] = value
	}
	return out
}
