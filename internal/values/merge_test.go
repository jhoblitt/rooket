package values

import (
	"reflect"
	"testing"
)

func TestMerge(t *testing.T) {
	tests := []struct {
		name   string
		layers []Layer
		want   map[string]any
	}{
		{
			name: "later scalar wins",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"a": 1}},
				{Name: "top", Values: map[string]any{"a": 2}},
			},
			want: map[string]any{"a": 2},
		},
		{
			name: "maps merge by key",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"m": map[string]any{"a": 1, "b": 2}}},
				{Name: "top", Values: map[string]any{"m": map[string]any{"b": 3}}},
			},
			want: map[string]any{"m": map[string]any{"a": 1, "b": 3}},
		},
		{
			name: "null deletes",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"a": 1, "b": 2}},
				{Name: "top", Values: map[string]any{"a": nil}},
			},
			want: map[string]any{"b": 2},
		},
		{
			name: "named lists merge by name",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"pools": []any{
					map[string]any{"name": "one", "size": 3},
				}}},
				{Name: "top", Values: map[string]any{"pools": []any{
					map[string]any{"name": "two", "size": 1},
				}}},
			},
			want: map[string]any{"pools": []any{
				map[string]any{"name": "one", "size": 3},
				map[string]any{"name": "two", "size": 1},
			}},
		},
		{
			name: "matching names deep-merge in place",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"pools": []any{
					map[string]any{"name": "one", "size": 3, "keep": true},
				}}},
				{Name: "top", Values: map[string]any{"pools": []any{
					map[string]any{"name": "one", "size": 1},
				}}},
			},
			want: map[string]any{"pools": []any{
				map[string]any{"name": "one", "size": 1, "keep": true},
			}},
		},
		{
			name: "unnamed lists replace",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"l": []any{1, 2, 3}}},
				{Name: "top", Values: map[string]any{"l": []any{9}}},
			},
			want: map[string]any{"l": []any{9}},
		},
		{
			name: "duplicate names disqualify name merging",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"l": []any{
					map[string]any{"name": "dup"}, map[string]any{"name": "dup"},
				}}},
				{Name: "top", Values: map[string]any{"l": []any{
					map[string]any{"name": "new"},
				}}},
			},
			want: map[string]any{"l": []any{map[string]any{"name": "new"}}},
		},
		{
			name: "null clears a named list before re-adding",
			layers: []Layer{
				{Name: "base", Values: map[string]any{"pools": []any{
					map[string]any{"name": "one"},
				}}},
				{Name: "mid", Values: map[string]any{"pools": nil}},
				{Name: "top", Values: map[string]any{"pools": []any{
					map[string]any{"name": "only"},
				}}},
			},
			want: map[string]any{"pools": []any{map[string]any{"name": "only"}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Merge(tc.layers)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestMergeProvenance(t *testing.T) {
	_, prov := Merge([]Layer{
		{Name: "base", Values: map[string]any{
			"m":     map[string]any{"a": 1, "b": 2},
			"pools": []any{map[string]any{"name": "one", "size": 3}},
		}},
		{Name: "profile:rgw", Values: map[string]any{
			"m":     map[string]any{"b": 3},
			"pools": []any{map[string]any{"name": "one", "size": 1}},
		}},
	})

	want := map[string]string{
		"m.a":               "base",
		"m.b":               "profile:rgw",
		"pools[one].name":   "profile:rgw",
		"pools[one].size":   "profile:rgw",
	}
	for path, layer := range want {
		if prov[path] != layer {
			t.Errorf("provenance[%q] = %q, want %q", path, prov[path], layer)
		}
	}
}

func TestMergeDoesNotAliasInput(t *testing.T) {
	src := map[string]any{"l": []any{1, 2}}
	got, _ := Merge([]Layer{{Name: "only", Values: src}})
	got["l"].([]any)[0] = 99
	if src["l"].([]any)[0] != 1 {
		t.Error("Merge aliased the input layer's slice")
	}
}
