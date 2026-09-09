package codeindex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
)

// ExportNDJSON writes a workspace's index (or a single branch's file/state
// set plus all needed blobs) as an ndjson stream suitable for
// `skopos index export`. Blob payloads are the original parsed-file JSON, so
// imports re-materialize symbols/edges through the normal path.
func (s *Service) ExportNDJSON(workspace, branch string, w io.Writer) error {
	db, err := s.store.DB(workspace)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)

	// Which hashes does the export need?
	needed := map[string]bool{}
	fileRows, err := db.Query(`SELECT branch, path, hash FROM branch_files`+branchFilter(branch), branchArgs(branch)...)
	if err != nil {
		return err
	}
	type fileRec struct {
		Type   string `json:"type"`
		Branch string `json:"branch"`
		Path   string `json:"path"`
		Hash   string `json:"hash"`
	}
	var files []fileRec
	for fileRows.Next() {
		var f fileRec
		f.Type = "file"
		if err := fileRows.Scan(&f.Branch, &f.Path, &f.Hash); err != nil {
			fileRows.Close()
			return err
		}
		needed[f.Hash] = true
		files = append(files, f)
	}
	fileRows.Close()
	if err := fileRows.Err(); err != nil {
		return err
	}

	// State rows.
	stateRows, err := db.Query(`SELECT branch, head_sha, built_at, source, file_count, symbol_count FROM state`+branchFilter(branch), branchArgs(branch)...)
	if err != nil {
		return err
	}
	type stateRec struct {
		Type    string `json:"type"`
		Branch  string `json:"branch"`
		HeadSHA string `json:"head_sha"`
		BuiltAt string `json:"built_at"`
		Source  string `json:"source"`
	}
	var states []stateRec
	for stateRows.Next() {
		var st stateRec
		st.Type = "state"
		var fileCount, symbolCount int
		if err := stateRows.Scan(&st.Branch, &st.HeadSHA, &st.BuiltAt, &st.Source, &fileCount, &symbolCount); err != nil {
			stateRows.Close()
			return err
		}
		states = append(states, st)
	}
	stateRows.Close()
	if err := stateRows.Err(); err != nil {
		return err
	}

	// Blobs referenced by the exported file set.
	blobRows, err := db.Query(`SELECT hash, payload FROM blobs`)
	if err != nil {
		return err
	}
	type blobRec struct {
		Type    string `json:"type"`
		Hash    string `json:"hash"`
		Payload string `json:"payload"`
	}
	var blobs []blobRec
	for blobRows.Next() {
		var b blobRec
		b.Type = "blob"
		if err := blobRows.Scan(&b.Hash, &b.Payload); err != nil {
			blobRows.Close()
			return err
		}
		if needed[b.Hash] {
			blobs = append(blobs, b)
		}
	}
	blobRows.Close()
	if err := blobRows.Err(); err != nil {
		return err
	}

	for _, b := range blobs {
		if err := enc.Encode(b); err != nil {
			return err
		}
	}
	for _, f := range files {
		if err := enc.Encode(f); err != nil {
			return err
		}
	}
	for _, st := range states {
		if err := enc.Encode(st); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func branchFilter(branch string) string {
	if branch == "" {
		return ""
	}
	return " WHERE branch = ?"
}

func branchArgs(branch string) []any {
	if branch == "" {
		return nil
	}
	return []any{branch}
}

// ImportNDJSON reads an export stream into a workspace's index DB. Replays
// blobs through AddBlob (idempotent, re-materializes symbols/edges) and file
// sets through Commit (atomic per branch).
func (s *Service) ImportNDJSON(workspace string, r io.Reader) (branches []string, err error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 32<<20)

	filesByBranch := map[string][]FileEntry{}
	stateByBranch := map[string]struct {
		HeadSHA, BuiltAt, Source string
	}{}
	for scanner.Scan() {
		var head struct {
			Type    string `json:"type"`
			Hash    string `json:"hash"`
			Payload string `json:"payload"`
			Branch  string `json:"branch"`
			Path    string `json:"path"`
			HeadSHA string `json:"head_sha"`
			BuiltAt string `json:"built_at"`
			Source  string `json:"source"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &head); err != nil {
			return nil, fmt.Errorf("malformed export line: %w", err)
		}
		switch head.Type {
		case "blob":
			var blobRow struct {
				Hash    string `json:"hash"`
				Payload string `json:"payload"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &blobRow); err != nil {
				return nil, err
			}
			var fileRes parse.FileResult
			if err := json.Unmarshal([]byte(blobRow.Payload), &fileRes); err != nil {
				return nil, fmt.Errorf("blob %s: %w", blobRow.Hash, err)
			}
			if err := s.store.AddBlob(workspace, &fileRes); err != nil {
				return nil, err
			}
		case "file":
			filesByBranch[head.Branch] = append(filesByBranch[head.Branch], FileEntry{Path: head.Path, Hash: head.Hash})
		case "state":
			stateByBranch[head.Branch] = struct{ HeadSHA, BuiltAt, Source string }{head.HeadSHA, head.BuiltAt, head.Source}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	for branch, files := range filesByBranch {
		st := stateByBranch[branch]
		if err := s.store.Commit(workspace, branch, st.HeadSHA, st.Source, files); err != nil {
			return nil, err
		}
		branches = append(branches, branch)
	}
	return branches, nil
}
