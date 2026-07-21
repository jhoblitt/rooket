package values

import "fmt"

// Layer is one contributor to a chart's values, named for provenance reporting.
type Layer struct {
	Name   string
	Values map[string]any
}

// Merge folds layers left to right, later layers winning, and returns the
// merged values alongside a map from each leaf path to the layer that set it.
//
// Maps merge by key and an explicit nil deletes. A list merges by the "name"
// field of its elements when both sides qualify (see namedList); every other
// list replaces, which is Helm's behaviour. Name-keyed merging is what lets two
// profiles contribute to the same list — under replace semantics the second
// would silently drop the first's entries.
func Merge(layers []Layer) (map[string]any, map[string]string) {
	out := map[string]any{}
	prov := map[string]string{}
	for _, l := range layers {
		mergeMap(out, l.Values, "", l.Name, prov)
	}
	return out, prov
}

func mergeMap(dst, src map[string]any, path, layer string, prov map[string]string) {
	for k, v := range src {
		p := k
		if path != "" {
			p = path + "." + k
		}
		switch tv := v.(type) {
		case nil:
			delete(dst, k)
			delete(prov, p)
		case map[string]any:
			sub, ok := dst[k].(map[string]any)
			if !ok {
				sub = map[string]any{}
				dst[k] = sub
			}
			mergeMap(sub, tv, p, layer, prov)
		case []any:
			if cur, ok := dst[k].([]any); ok && namedList(cur) && namedList(tv) {
				dst[k] = mergeNamed(cur, tv, p, layer, prov)
				continue
			}
			dst[k] = deepCopy(tv)
			prov[p] = layer
		default:
			dst[k] = v
			prov[p] = layer
		}
	}
}

// namedList reports whether every element is a map carrying a unique string
// "name", the condition under which two lists can be merged element-wise.
func namedList(l []any) bool {
	if len(l) == 0 {
		return false
	}
	seen := make(map[string]bool, len(l))
	for _, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			return false
		}
		n, ok := m["name"].(string)
		if !ok || seen[n] {
			return false
		}
		seen[n] = true
	}
	return true
}

func mergeNamed(dst, src []any, path, layer string, prov map[string]string) []any {
	idx := make(map[string]int, len(dst))
	out := make([]any, 0, len(dst)+len(src))
	for i, e := range dst {
		idx[e.(map[string]any)["name"].(string)] = i
		out = append(out, deepCopy(e))
	}
	for _, e := range src {
		em := e.(map[string]any)
		name := em["name"].(string)
		p := fmt.Sprintf("%s[%s]", path, name)
		if i, ok := idx[name]; ok {
			mergeMap(out[i].(map[string]any), em, p, layer, prov)
			continue
		}
		nm := map[string]any{}
		mergeMap(nm, em, p, layer, prov)
		out = append(out, nm)
	}
	return out
}

func deepCopy(v any) any {
	switch tv := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(tv))
		for k, e := range tv {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(tv))
		for i, e := range tv {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}
