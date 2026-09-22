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

// readWorkspaceBrief validates that path is a markdown file within workspace,
// opens it safely via os.OpenRoot, and returns its normalized relative path and content.
func readWorkspaceBrief(workspace, path string) (relPath, content string, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", errors.New("brief path is required")
	}
	if strings.ToLower(filepath.Ext(path)) != ".md" {
		return "", "", errors.New("brief must be a markdown (.md) file")
	}

	ws, err := checkWorkspace(workspace)
	if err != nil {
		return "", "", err
	}

	var rel string
	if filepath.IsAbs(path) {
		r, err := filepath.Rel(ws, path)
		if err != nil || strings.HasPrefix(r, "..") || filepath.IsAbs(r) {
			return "", "", errors.New("brief file must be inside the workspace folder")
		}
		rel = r
	} else {
		rel = path
	}
	rel = filepath.Clean(rel)
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", "", errors.New("brief file must be inside the workspace folder")
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
		return "", "", errors.New("brief must be a regular file")
	}
	if fi.Size() > 2<<20 {
		return "", "", errors.New("brief file is too large (max 2MB)")
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return "", "", err
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
		ws = s.DefaultWorkspace
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
