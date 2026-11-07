package codeindex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
)

// PushOptions configure Push. Report (optional) gets parse progress; Cache
// (optional) skips parsing files unchanged since the last build.
type PushOptions struct {
	ServerURL, APIKey string
	Workspace, Branch string
	Root              string // the checkout to index
	Source            string // recorded with the branch ("push:<host>", "worker:<id>")
	Cache             BuildCacher
	Report            func(done, total int)
	HTTPClient        *http.Client
}

// PushResult says what a push did.
type PushResult struct {
	Head     string // the commit indexed
	Files    int    // files in the branch index
	Uploaded int    // blobs the server did not have yet
}

// Push builds the checkout at opts.Root and pushes its branch index to a
// skopos server: a manifest (which parsed blobs does the server lack?), the
// missing blobs as ndjson in bounded chunks, then the branch commit. Used by
// `skopos index push` and by idle workers (tmp/agent-mcp-and-indexing —
// design.md §3).
func Push(ctx context.Context, opts PushOptions) (PushResult, error) {
	var res PushResult
	results, head, err := BuildWithCache(ctx, parse.NewExtractor(), opts.Root, opts.Branch, opts.Report, opts.Cache)
	if err != nil {
		return res, err
	}
	res.Head = head
	entries := make([]FileEntry, 0, len(results))
	byHash := map[string]*parse.FileResult{}
	for _, r := range results {
		entries = append(entries, FileEntry{Path: r.Path, Hash: r.Hash})
		byHash[r.Hash] = r
	}
	res.Files = len(entries)
	base := "/api/codeindex/" + url.PathEscape(opts.Workspace)

	var manifest struct {
		Missing []string `json:"missing"`
	}
	if err := pushCall(ctx, opts, base+"/manifest", map[string]any{"files": entries}, "application/json", &manifest); err != nil {
		return res, fmt.Errorf("manifest: %w", err)
	}

	// Bounded chunks: a large repo must not build one giant request (the
	// server caps each at 256 MiB), and client memory stays flat.
	const chunkBlobs = 512
	var buf bytes.Buffer
	chunk := 0
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		if err := pushCall(ctx, opts, base+"/blobs", append([]byte(nil), buf.Bytes()...), "application/x-ndjson", nil); err != nil {
			return fmt.Errorf("blobs: %w", err)
		}
		res.Uploaded += chunk
		chunk = 0
		buf.Reset()
		return nil
	}
	enc := json.NewEncoder(&buf)
	for _, h := range manifest.Missing {
		r, ok := byHash[h]
		if !ok {
			continue
		}
		if err := enc.Encode(r); err != nil {
			return res, err
		}
		chunk++
		if chunk >= chunkBlobs || buf.Len() >= 32<<20 {
			if err := flush(); err != nil {
				return res, err
			}
		}
	}
	if err := flush(); err != nil {
		return res, err
	}
	if err := pushCall(ctx, opts, base+"/commit", map[string]any{
		"branch": opts.Branch, "head_sha": head, "source": opts.Source, "files": entries,
	}, "application/json", nil); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

// pushCall POSTs body (JSON-encoded unless already bytes) and decodes the
// response into out (nil = discard).
func pushCall(ctx context.Context, opts PushOptions, path string, body any, contentType string, out any) error {
	payload, ok := body.([]byte)
	if !ok {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = raw
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(opts.ServerURL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	if opts.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+opts.APIKey)
	}
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(msg, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
