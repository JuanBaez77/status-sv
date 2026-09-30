//go:build !unix

package runner

import "os/exec"

// isolate no hace nada fuera de Unix: ahí el agent solo se usa para
// desarrollar, y exec.CommandContext ya mata al proceso.
func isolate(*exec.Cmd) {}
