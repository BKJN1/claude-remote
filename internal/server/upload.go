package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"claude-remote/internal/auth"
)

// UploadDir is the only place uploads can go, inside the session's project.
const UploadDir = ".claude-remote-uploads"

const (
	maxUpload = 10 << 20
	// maxUploadFiles bounds disk use per project (no quota otherwise).
	maxUploadFiles = 200
)

// allowedUploads maps a sniffed content type to the extension we store it
// under. The type comes from the file's bytes, not from the client's claim,
// and the stored name is random: the client controls neither name nor path.
var allowedUploads = map[string]string{
	"image/png":                 "png",
	"image/jpeg":                "jpg",
	"image/gif":                 "gif",
	"image/webp":                "webp",
	"application/pdf":           "pdf",
	"text/plain; charset=utf-8": "txt",
}

// handleUpload stores one file in <project>/.claude-remote-uploads/ and
// returns its relative path, which the page then types into the terminal.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.o.Sessions.Get(r.URL.Query().Get("session"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no_such_session")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "too_large")
			return
		}
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "empty")
		return
	}
	ext, ok := allowedUploads[http.DetectContentType(data)]
	if !ok {
		writeErr(w, http.StatusUnsupportedMediaType, "unsupported_type")
		return
	}
	dir := filepath.Join(entry.Workdir, UploadDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.o.Log.Error("upload dir failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) >= maxUploadFiles {
		writeErr(w, http.StatusInsufficientStorage, "too_many_files")
		return
	}
	// Keep uploads out of the user's git history.
	if gi := filepath.Join(dir, ".gitignore"); fileMissing(gi) {
		os.WriteFile(gi, []byte("*\n"), 0o600)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	name := hex.EncodeToString(b) + "." + ext
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.o.Log.Error("upload create failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(filepath.Join(dir, name))
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	s.o.Log.Info("upload", "ip", auth.ClientIP(r), "session", entry.ID, "bytes", len(data), "type", ext)
	writeJSON2(w, http.StatusCreated, map[string]string{"path": UploadDir + "/" + name})
}

func fileMissing(p string) bool {
	_, err := os.Stat(p)
	return errors.Is(err, os.ErrNotExist)
}
