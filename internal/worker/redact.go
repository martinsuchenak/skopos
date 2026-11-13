package worker

import (
	"bufio"
	"bytes"
	"context"
	"net/url"
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
	return w.fileSecretsIn(workspace, repo.Path)
}

// fileSecretsIn reads the same files under dir: the tree a stage works in
// when that is not the clone (a worktree), and the clone again once setup
// has run there — a file that setup writes or copies holds values the read
// before the run never saw.
func (w *Worker) fileSecretsIn(workspace, dir string) []string {
	repo := w.repo(workspace)
	if repo == nil || dir == "" {
		return nil
	}
	var out []string
	for _, f := range repo.RedactFiles {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f)))
		if err != nil || len(raw) > redactFileMax {
			continue
		}
		out = append(out, envSecrets(raw)...)
	}
	return out
}

// addRunSecrets adds values to the run's redactor after its log opened.
func (w *Worker) addRunSecrets(ctx context.Context, values []string) {
	if len(values) == 0 {
		return
	}
	w.runSecrets = append(w.runSecrets, values...)
	runLogFrom(ctx).setRedact(w.redactor())
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
		if !ok {
			continue
		}
		value = envValue(value)
		if value == "" {
			continue
		}
		if credentialKey(key) {
			out = append(out, value)
			continue
		}
		// A password inside a URL (DATABASE_URL=postgres://app:secret@db/x),
		// whatever the variable is called.
		if u, err := url.Parse(value); err == nil && u.User != nil {
			if pw, ok := u.User.Password(); ok && pw != "" {
				out = append(out, pw)
			}
		}
	}
	return out
}

// envValue is the value of an env-file line as a shell or a dotenv reader
// takes it: the text inside its quotes, or the unquoted text up to a
// comment. `"p4ss word" # staging` and `hunter2 # staging` are the password
// alone — kept with the comment, the value would never match what an agent
// prints.
func envValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if q := v[0]; q == '"' || q == '\'' {
		for i := 1; i < len(v); i++ {
			if v[i] == '\\' && q == '"' && i+1 < len(v) {
				i++
				continue
			}
			if v[i] == q {
				inner := v[1:i]
				if q == '"' {
					inner = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(inner)
				}
				return inner
			}
		}
		return v[1:] // no closing quote: everything after the opening one
	}
	for _, sep := range []string{" #", "\t#"} {
		if i := strings.Index(v, sep); i >= 0 {
			v = v[:i]
		}
	}
	return strings.TrimSpace(v)
}

// credentialKey reports a variable name that holds a credential by the
// look of it.
func credentialKey(key string) bool {
	up := strings.ToUpper(strings.TrimSpace(key))
	for _, part := range []string{"PASSWORD", "PASSWD", "PASSPHRASE", "SECRET", "TOKEN", "CREDENTIAL", "APIKEY", "API_KEY", "PRIVATE_KEY"} {
		if strings.Contains(up, part) {
			return true
		}
	}
	for _, suffix := range []string{"_KEY", "_PASS", "_PWD"} {
		if strings.HasSuffix(up, suffix) {
			return true
		}
	}
	return up == "KEY" || up == "PASS"
}
