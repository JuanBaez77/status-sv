package main

import (
	"strings"
	"testing"
)

func TestReadPasswordLine(t *testing.T) {
	tests := map[string]string{
		"con salto de línea":  "una contraseña larga\n",
		"con CRLF de Windows": "una contraseña larga\r\n",
		"sin salto":           "una contraseña larga",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := readPasswordLine(strings.NewReader(in))
			if err != nil || got != "una contraseña larga" {
				t.Errorf("readPasswordLine(%q) = %q, %v", in, got, err)
			}
		})
	}

	// Solo la primera línea: lo que venga después no es parte de la contraseña.
	got, err := readPasswordLine(strings.NewReader("primera\nsegunda\n"))
	if err != nil || got != "primera" {
		t.Errorf("readPasswordLine con dos líneas = %q, %v", got, err)
	}
}
