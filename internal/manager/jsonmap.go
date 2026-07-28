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
// Shallow is deliberate: recreate rewrites top-level keys and hands the nested
// values straight back to Docker as they came out of inspect, so deep-copying
// would only cost time. The one nested map that does get modified — Config.Volumes
// — is cloned explicitly at the point of use.
func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for key, value := range m {
		out[key] = value
	}
	return out
}
