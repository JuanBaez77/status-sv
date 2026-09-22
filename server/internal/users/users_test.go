package users

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUsernames(t *testing.T) {
	tests := []struct {
		in    string
		want  string
		valid bool
	}{
		{"juan", "juan", true},
		{"  Juan ", "juan", true},
		{"juan.baez-77_x", "juan.baez-77_x", true},
		{"ju", "ju", false},
		{".juan", ".juan", false},
		{"juan baez", "juan baez", false},
		{"juán", "juán", false},
		{strings.Repeat("a", 33), strings.Repeat("a", 33), false},
	}
	for _, tt := range tests {
		got := NormalizeUsername(tt.in)
		if got != tt.want {
			t.Errorf("NormalizeUsername(%q) = %q, se esperaba %q", tt.in, got, tt.want)
		}
		if ValidUsername(got) != tt.valid {
			t.Errorf("ValidUsername(%q) = %v, se esperaba %v", got, !tt.valid, tt.valid)
		}
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"entra", "Mozilla/5.0", 64, "Mozilla/5.0"},
		{"corta en bytes", "abcdef", 4, "abcd"},
		// "ñ" son dos bytes: cortar en el medio lo deja afuera entero.
		{"no parte un carácter", "aañ", 3, "aa"},
		{"saca lo inválido", "a\xffb", 64, "ab"},
		{"saca el nulo", "a\x00b", 64, "ab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.in, tt.limit)
			if got != tt.want || !utf8.ValidString(got) {
				t.Errorf("truncate(%q, %d) = %q, se esperaba %q", tt.in, tt.limit, got, tt.want)
			}
		})
	}
}
