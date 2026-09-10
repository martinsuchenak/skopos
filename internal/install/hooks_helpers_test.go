package install

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runWithEnv executes a hook script with stdin and extra env, returning
// stdout (hooks print their JSON envelope there). argsOrStdin is stdin.
func runWithEnv(t *testing.T, pathOrCmd string, stdin string, extraEnv []string, dir string) (string, error) {
	t.Helper()
	var cmd *exec.Cmd
	if strings.Contains(pathOrCmd, " ") {
		parts := strings.SplitN(pathOrCmd, " ", 2)
		cmd = exec.Command(parts[0], strings.Fields(parts[1])...)
	} else {
		cmd = exec.Command(pathOrCmd)
	}
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	env := os.Environ()
	env = append(env, extraEnv...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil && stderr.Len() > 0 {
		return stdout.String(), err
	}
	return stdout.String(), err
}
