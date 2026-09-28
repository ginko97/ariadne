package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// maxBriefBytes is the largest task file that is read, listed or run.
const maxBriefBytes = 2 << 20

type briefRequest struct {
	Path      string `json:"path"`
	Workspace string `json:"workspace,omitempty"`
}

type briefResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// SHA256 names the text shown, so starting the brief can prove it is
	// running the same one (chatRequest.BriefSHA256).
	SHA256 string `json:"sha256"`
}

// briefDigest is the hex SHA-256 of a brief's text as read from disk.
func briefDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// briefPath checks that path names a markdown file inside workspace, and
// returns the folder and the path relative to it. Only the shape of the path:
// whether the file exists, and whether a link leads out, is for os.Root to
// say when the file is opened.
func briefPath(workspace, path string) (ws, rel string, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", errors.New("a task file path is required")
	}
	if strings.ToLower(filepath.Ext(path)) != ".md" {
		return "", "", errors.New("a task file must be a markdown (.md) file")
	}
	if ws, err = checkWorkspace(workspace); err != nil {
		return "", "", err
	}
	rel = path
	if filepath.IsAbs(path) {
		r, err := filepath.Rel(ws, path)
		if err != nil || strings.HasPrefix(r, "..") || filepath.IsAbs(r) {
			return "", "", errors.New("the task file must be inside the conversation's folder")
		}
		rel = r
	}
	// IsLocal is the rule per platform: on Windows it also refuses a rooted
	// "\x", a drive-relative "C:x" and an "x:stream" (all names os.Root would
	// refuse too, but not with this message); elsewhere a colon is just a
	// character, and "q3: notes.md" is a name.
	rel = filepath.Clean(rel)
	if !filepath.IsLocal(rel) {
		return "", "", errors.New("the task file must be inside the conversation's folder")
	}
	return ws, rel, nil
}

// readWorkspaceBrief validates that path is a markdown file within workspace,
// opens it safely via os.OpenRoot, and returns its normalized relative path and content.
func readWorkspaceBrief(workspace, path string) (relPath, content string, err error) {
	ws, rel, err := briefPath(workspace, path)
	if err != nil {
		return "", "", err
	}

	root, err := os.OpenRoot(ws)
	if err != nil {
		return "", "", err
	}
	defer root.Close()

	f, err := root.Open(rel)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		return "", "", errors.New("a task file must be a regular file")
	}
	if fi.Size() > maxBriefBytes {
		return "", "", errors.New("the task file is too large (max 2MB)")
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", "", errors.New("the task file is empty")
	}

	return filepath.ToSlash(rel), string(data), nil
}

func (s *Server) handleBrief(w http.ResponseWriter, r *http.Request) {
	var req briefRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed request body")
		return
	}

	ws := req.Workspace
	if ws == "" {
		ws = s.DefaultFolder()
	}
	if ws == "" {
		if cwd, err := os.Getwd(); err == nil {
			ws = cwd
		}
	}

	relPath, content, err := readWorkspaceBrief(ws, req.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(briefResponse{
		Path:    relPath,
		Content: content,
		SHA256:  briefDigest(content),
	})
}
