package runner

import (
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "texto común", in: "Step 3/8 : RUN pip install -r requirements.txt", max: 200, want: "Step 3/8 : RUN pip install -r requirements.txt"},
		{name: "colores", in: "\x1b[32mOK\x1b[0m listo", max: 200, want: "OK listo"},
		{name: "tabulador", in: "a\tb", max: 200, want: "a b"},
		{name: "control", in: "a\x07b\x00c", max: 200, want: "abc"},
		{name: "utf-8 inválido", in: "a\xffb", max: 200, want: "a?b"},
		{name: "recorte sin partir una letra", in: "ñññ", max: 5, want: "ññ"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sanitize(tt.in, tt.max); got != tt.want {
				t.Errorf("Sanitize(%q) = %q, se esperaba %q", tt.in, got, tt.want)
			}
		})
	}
}

// El secreto se enmascara antes de recortar: el recorte no puede dejar media
// contraseña a la vista.
func TestSanitizeMasksBeforeTruncating(t *testing.T) {
	secret := "hunter2-muy-largo-y-secreto"
	got := Sanitize("DATABASE_PASSWORD="+secret, 24)
	if strings.Contains(got, "hunter") {
		t.Errorf("Sanitize dejó ver parte del secreto: %q", got)
	}
}

func TestTailKeepsTheLastLines(t *testing.T) {
	tail := NewTail(2, 10)
	for _, line := range []string{"uno", "", "   ", "dos", "tres renglón largo"} {
		tail.Add(line)
	}
	got := tail.Lines()
	if len(got) != 2 || got[0] != "dos" || len(got[1]) > 10 {
		t.Errorf("Lines = %q", got)
	}

	var nilTail *Tail
	nilTail.Add("no rompe")
	if nilTail.Lines() != nil {
		t.Error("un Tail nil devolvió renglones")
	}
}
