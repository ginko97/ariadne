package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Writing task files from the page: saving an edit to one, and saving a new
// one the model drafted. The person is the author either way — they read and
// changed the text in the page — so neither asks the way the model's own
// write_file does. Both are behind the CSRF token, and both write only .md
// files inside the conversation's folder, through os.Root.

var (
	// errBriefChanged: the file on disk is not the one the edit started from.
	errBriefChanged = errors.New("the task file changed on disk since it was opened; reopen it to see the new text before saving")
	// errBriefExists: a new task file would replace one that already exists.
	errBriefExists = errors.New("a task file with that name already exists; choose another name")
)

type briefSaveRequest struct {
	Path      string `json:"path"`
	Workspace string `json:"workspace,omitempty"`
	Content   string `json:"content"`
	// SHA256 is the digest of the text the edit started from (POST
	// /api/brief), required when changing an existing file.
	SHA256 string `json:"sha256,omitempty"`
	// Create makes a new file, and refuses one that exists.
	Create bool `json:"create,omitempty"`
}

// checkBriefContent is what any task file must be, however it is written.
func checkBriefContent(content string) error {
	switch {
	case strings.TrimSpace(content) == "":
		return errors.New("the task file is empty")
	case len(content) > maxBriefBytes:
		return errors.New("the task file is too large (max 2MB)")
	case !utf8.ValidString(content):
		return errors.New("the task file is not valid text")
	}
	return nil
}

// saveWorkspaceBrief writes content to the task file at path in workspace
// and returns its relative path and new digest.
//
// Changing an existing file needs wantSHA, the digest of the text the edit
// started from: if the file changed since — in Notepad, say, or by a
// conversation's edit_file — the save is refused rather than silently
// overwriting either version. The replacement is atomic, a temp file renamed
// over the old one, and keeps the file's permissions. Creating refuses an
// existing name. Neither creates folders: a task file goes where one could
// already be listed.
func saveWorkspaceBrief(workspace, path, content, wantSHA string, create bool) (rel, sha string, err error) {
	if err := checkBriefContent(content); err != nil {
		return "", "", err
	}
	ws, rel, err := briefPath(workspace, path)
	if err != nil {
		return "", "", err
	}
	root, err := os.OpenRoot(ws)
	if err != nil {
		return "", "", err
	}
	defer root.Close()

	if create {
		f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			return "", "", errBriefExists
		}
		if err != nil {
			return "", "", err
		}
		if _, err := f.WriteString(content); err != nil {
			f.Close()
			_ = root.Remove(rel)
			return "", "", err
		}
		if err := f.Close(); err != nil {
			return "", "", err
		}
		return filepath.ToSlash(rel), briefDigest(content), nil
	}

	fi, err := root.Stat(rel)
	if err != nil {
		return "", "", err
	}
	if !fi.Mode().IsRegular() {
		return "", "", errors.New("a task file must be a regular file")
	}
	current, err := root.ReadFile(rel)
	if err != nil {
		return "", "", err
	}
	if wantSHA == "" || briefDigest(string(current)) != wantSHA {
		return "", "", errBriefChanged
	}
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	tmp := filepath.Join(filepath.Dir(rel), ".ariadne-task-"+hex.EncodeToString(suffix[:]))
	if err := root.WriteFile(tmp, []byte(content), fi.Mode().Perm()); err != nil {
		return "", "", err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return "", "", err
	}
	return filepath.ToSlash(rel), briefDigest(content), nil
}

func (s *Server) handleBriefSave(w http.ResponseWriter, r *http.Request) {
	var req briefSaveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBriefBytes+(64<<10))).Decode(&req); err != nil {
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
	rel, sha, err := saveWorkspaceBrief(ws, req.Path, req.Content, req.SHA256, req.Create)
	switch {
	case errors.Is(err, errBriefChanged), errors.Is(err, errBriefExists):
		httpError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, briefResponse{Path: rel, Content: req.Content, SHA256: sha})
}

// maxDraftRequest bounds the description a draft is asked for.
const maxDraftRequest = 4000

type draftRequest struct {
	Description string `json:"description"`
	Workspace   string `json:"workspace,omitempty"`
}

// handleTaskDraft asks the model for a new task file from a description. The
// draft comes back as text for the page's editor; nothing is written until
// the person reads it and saves it (handleBriefSave with create). How the
// model is asked — with no tools at all, so it reads nothing while it writes
// what will later run as the person's own instruction — is DraftTask's job.
func (s *Server) handleTaskDraft(w http.ResponseWriter, r *http.Request) {
	if s.DraftTask == nil {
		httpError(w, http.StatusNotImplemented, "drafting a task is not available here")
		return
	}
	var req draftRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "malformed request body")
		return
	}
	desc := strings.TrimSpace(req.Description)
	switch {
	case desc == "":
		httpError(w, http.StatusBadRequest, "describe the task first")
		return
	case utf8.RuneCountInString(desc) > maxDraftRequest:
		httpError(w, http.StatusBadRequest, "the description is too long; keep it to a few sentences")
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
	if _, err := checkWorkspace(ws); err != nil {
		httpError(w, http.StatusBadRequest, "folder: "+err.Error())
		return
	}
	runID, text, err := s.DraftTask(r.Context(), ws, desc)
	if err != nil {
		httpError(w, http.StatusBadGateway, "the model could not draft the task: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": runID, "content": text})
}

// DraftFunc drafts a task file for description in folder, as one
// conversation that is recorded like any other, and returns its id and the
// draft.
type DraftFunc func(ctx context.Context, folder, description string) (runID, text string, err error)
