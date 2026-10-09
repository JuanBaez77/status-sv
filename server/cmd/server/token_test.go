package main

import (
	"strings"
	"testing"
)

// Los argumentos se validan antes de tocar la configuración o la base: un
// error de tipeo no puede terminar revocando todo.
func TestTokenRevokeArgs(t *testing.T) {
	tests := map[string]struct {
		args []string
		want string
	}{
		"sin nada":        {args: nil, want: "hace falta --id o --all"},
		"los dos":         {args: []string{"--id", "0192f5e0-0000-7000-8000-000000000000", "--all"}, want: "hace falta --id o --all"},
		"id que no es":    {args: []string{"--id", "svmcp_abc"}, want: "no es un UUID"},
		"flag que no hay": {args: []string{"--todos"}, want: "flag provided but not defined"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := runTokenRevoke(t.Context(), tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("runTokenRevoke(%q) = %v, se esperaba %q", tt.args, err, tt.want)
			}
		})
	}
}
