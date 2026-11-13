package worker

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// redactFileMax bounds a redact_files file: an env file, not a data set.
const redactFileMax = 1 << 20

// fileSecrets reads the credentials in a repository's redact_files: the
// values of the credential-like keys of their KEY=VALUE lines. They join
// the run's redactor (which skips values under 8 characters), so a QA
// agent that reads the file to sign in to a test system never writes a
// password into the run log. A missing or unreadable file gives nothing:
// whether the run can work without it is the run's to find out.
func (w *Worker) fileSecrets(workspace string) []string {
	repo := w.repo(workspace)
	if repo == nil {
		return nil
	}
	var out []string
	for _, f := range repo.RedactFiles {
		raw, err := os.ReadFile(filepath.Join(repo.Path, filepath.FromSlash(f)))
		if err != nil || len(raw) > redactFileMax {
			continue
		}
		out = append(out, envSecrets(raw)...)
	}
	return out
}

// envSecrets returns the values of an env file's credential-like keys.
func envSecrets(raw []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), redactFileMax)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok || !credentialKey(key) {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

// credentialKey reports a variable name that holds a credential by the
// look of it.
func credentialKey(key string) bool {
	up := strings.ToUpper(strings.TrimSpace(key))
	for _, part := range []string{"PASSWORD", "PASSWD", "SECRET", "TOKEN", "CREDENTIAL"} {
		if strings.Contains(up, part) {
			return true
		}
	}
	return strings.HasSuffix(up, "_KEY") || up == "KEY" || strings.HasSuffix(up, "_PASS")
}
