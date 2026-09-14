package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/paularlott/cli"
)

func init() {
	Register(fixedCompletionCommand())
}

// fixedCompletionCommand wraps the library's completion command because its
// zsh output is broken for both standard install forms: it ends with
// `compdef _skopos skopos <absolute-invocation-path>` (a cwd-dependent path
// from filepath.Abs(os.Args[0]) that never matches the typed command), and
// it lacks the `#compdef skopos` first line that compinit requires to bind
// a file installed on fpath. The wrapper captures the generated script and
// fixes both; the hidden runtime query modes the script calls back into
// (--command/--flag/--value-flags) pass through untouched.
func fixedCompletionCommand() *cli.Command {
	c := cli.GenerateCompletionCommand()
	origRun := c.Run
	c.Run = func(ctx context.Context, cmd *cli.Command) error {
		// The shell is a named positional argument (GetStringArg, not
		// GetString); the hidden query modes arrive as flags. Pass
		// everything except plain zsh generation straight through.
		shell := cmd.GetStringArg("shell")
		if shell != "zsh" ||
			cmd.GetString("command") != "" || cmd.GetString("flag") != "" || cmd.GetString("value-flags") != "" {
			return origRun(ctx, cmd)
		}

		// Capture the library's stdout for the script generation path.
		r, w, err := os.Pipe()
		if err != nil {
			return origRun(ctx, cmd)
		}
		saved := os.Stdout
		os.Stdout = w
		runErr := origRun(ctx, cmd)
		os.Stdout = saved
		w.Close()
		out, readErr := io.ReadAll(r)
		if runErr != nil {
			return runErr
		}
		if readErr != nil {
			return fmt.Errorf("reading completion output: %w", readErr)
		}

		// The names key on the invoked binary's basename (the library
		// derives them from os.Args[0]), so derive them the same way.
		name := filepath.Base(os.Args[0])
		if name == "" || name == "." || strings.HasSuffix(name, ".exe") {
			name = "skopos"
		}
		lines := strings.Split(string(out), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.HasPrefix(lines[i], "compdef _"+name+" ") {
				lines[i] = "compdef _" + name + " " + name
				break
			}
		}
		script := "#compdef " + name + "\n" + strings.Join(lines, "\n")
		fmt.Print(script)
		return nil
	}
	return c
}
