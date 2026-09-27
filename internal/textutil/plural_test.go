package textutil

import "testing"

func TestPlural(t *testing.T) {
	tests := []struct {
		name     string
		n        int
		singular string
		plural   string
		want     string
	}{
		{name: "zero", n: 0, singular: "file", plural: "files", want: "0 files"},
		{name: "one", n: 1, singular: "file", plural: "files", want: "1 file"},
		{name: "two", n: 2, singular: "file", plural: "files", want: "2 files"},
		{name: "negative", n: -1, singular: "file", plural: "files", want: "-1 files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Plural(tt.n, tt.singular, tt.plural); got != tt.want {
				t.Errorf("Plural(%d, %q, %q) = %q, want %q", tt.n, tt.singular, tt.plural, got, tt.want)
			}
		})
	}
}
