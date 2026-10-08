package envfile

import (
	"errors"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	data := `# comentario
SECRET_KEY='va$lue "x" \n # no es comentario'
export DATABASE_URL="postgres://u:p@h/db"
SIN_COMILLAS=abc
VACIA=

renglón que no se entiende
=sin nombre
`
	vars := Parse([]byte(data))
	want := []Var{
		{"SECRET_KEY", `va$lue "x" \n # no es comentario`},
		{"DATABASE_URL", "postgres://u:p@h/db"},
		{"SIN_COMILLAS", "abc"},
		{"VACIA", ""},
	}
	if len(vars) != len(want) {
		t.Fatalf("Parse = %+v", vars)
	}
	for i := range want {
		if vars[i] != want[i] {
			t.Errorf("vars[%d] = %+v, se esperaba %+v", i, vars[i], want[i])
		}
	}
}

func TestMissing(t *testing.T) {
	data := []byte("A='1'\nB=''\nD='4'\n")
	got := Missing([]string{"A", "B", "C"}, data)
	if strings.Join(got, ",") != "B,C" {
		t.Errorf("Missing = %v", got)
	}
	if got := Missing([]string{"A"}, nil); len(got) != 1 {
		t.Errorf("sin archivo = %v", got)
	}
}

// Lo que se escribe se lee igual.
func TestFormatRoundTrip(t *testing.T) {
	vars := []Var{
		{"SECRET_KEY", `va$lue "x" \n # no es comentario`},
		{"UNICODE", "ñandú€"},
		{"VACIA", ""},
	}
	data, err := Format(vars)
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if !strings.Contains(string(data), `SECRET_KEY='va$lue "x" \n # no es comentario'`+"\n") {
		t.Errorf("Format:\n%s", data)
	}
	back := Parse(data)
	if len(back) != len(vars) {
		t.Fatalf("Parse(Format) = %+v", back)
	}
	for i := range vars {
		if back[i] != vars[i] {
			t.Errorf("%+v se leyó como %+v", vars[i], back[i])
		}
	}
}

func TestFormatRejects(t *testing.T) {
	for name, vars := range map[string][]Var{
		"comilla simple":   {{"A", "it's"}},
		"salto de línea":   {{"A", "a\nB=b"}},
		"retorno de carro": {{"A", "a\rb"}},
		"nombre inválido":  {{"a-b", "x"}},
		"repetida":         {{"A", "1"}, {"A", "2"}},
		"enorme":           {{"A", strings.Repeat("x", MaxValueLen+1)}},
	} {
		if _, err := Format(vars); err == nil {
			t.Errorf("%s: se escribió", name)
		}
	}
	if err := CheckValue("it's"); !errors.Is(err, ErrBadValue) {
		t.Errorf("CheckValue = %v", err)
	}
	if err := CheckValue("con\ttabulación"); err != nil {
		t.Errorf("una tabulación es un valor válido: %v", err)
	}
}

func TestMerge(t *testing.T) {
	current := []Var{{"A", "1"}, {"B", "2"}}
	got, err := Merge(current, []Var{{"B", "nuevo"}, {"C", "3"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Var{{"A", "1"}, {"B", "nuevo"}, {"C", "3"}}
	if len(got) != len(want) {
		t.Fatalf("Merge = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Merge[%d] = %+v", i, got[i])
		}
	}
	if current[1].Value != "2" {
		t.Error("Merge cambió el original")
	}
	if _, err := Merge([]Var{{"mal-nombre", "x"}}, nil); err == nil {
		t.Error("se aceptó reescribir un nombre que no se puede escribir")
	}
}
