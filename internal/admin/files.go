package admin

// File manager handlers: browse a directory, select files, upload, download,
// read/edit text files, create files/dirs, and delete. Restricted to FileRoot
// when set.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type fileEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Dir      bool   `json:"dir"`
	Size     int64  `json:"size"`
	ModTime  string `json:"mod_time"`
	Readable bool   `json:"readable"`
	Writable bool   `json:"writable"`
}

// resolvePath cleans a path and confines it within FileRoot (if set).
func (s *Server) resolvePath(p string) (string, error) {
	if p == "" {
		p = "/"
	}
	abs := filepath.Clean(p)
	if !filepath.IsAbs(abs) {
		abs = "/" + abs
	}
	if s.FileRoot != "" {
		root := filepath.Clean(s.FileRoot)
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("path outside allowed root %s", s.FileRoot)
		}
	}
	return abs, nil
}

func (s *Server) handleFilesPage(w http.ResponseWriter, r *http.Request) {
	// select mode: embed in config page to choose a path
	s.render(w, "files.html", map[string]any{
		"Root":       s.FileRoot,
		"SelectMode": r.URL.Query().Get("select") == "1",
		"Field":      r.URL.Query().Get("field"),
	})
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	abs, err := s.resolvePath(dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// build breadcrumb info
	segments := strings.Split(strings.Trim(filepath.Clean(abs), "/"), "/")
	crumbs := make([]crumb, 0, len(segments))
	cur := ""
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		cur = filepath.Join(cur, seg)
		if !filepath.IsAbs(cur) {
			cur = "/" + cur
		}
		crumbs = append(crumbs, crumb{Name: seg, Path: cur})
	}
	// allow browsing a parent of root
	list := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		fp := filepath.Join(abs, e.Name())
		info, ierr := e.Info()
		ent := fileEntry{Name: e.Name(), Path: fp, Dir: e.IsDir()}
		if ierr == nil {
			ent.Size = info.Size()
			ent.ModTime = info.ModTime().Format("2006-01-02 15:04")
		}
		list = append(list, ent)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Dir != list[j].Dir {
			return list[i].Dir
		}
		return list[i].Name < list[j].Name
	})
	writeJSON(w, http.StatusOK, struct {
		Path    string      `json:"path"`
		Crumbs  []crumb     `json:"crumbs"`
		Entries []fileEntry `json:"entries"`
	}{Path: abs, Crumbs: crumbs, Entries: list})
}

type crumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	abs, err := s.resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Disposition", "attachment; filename="+strconvQuote(filepath.Base(abs)))
	http.ServeContent(w, r, filepath.Base(abs), st.ModTime(), f)
}

func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request) {
	abs, err := s.resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": abs, "content": string(b)})
}

func (s *Server) handleFileWrite(w http.ResponseWriter, r *http.Request) {
	body, err := readBodyLimit(r, 8<<20)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	abs, err := s.resolvePath(req.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := atomicWrite(abs, []byte(req.Content)); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "saved"})
}

func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	dir, err := s.resolvePath(r.FormValue("dir"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	for _, fh := range r.MultipartForm.File["file"] {
		dst := filepath.Join(dir, fh.Filename)
		if _, derr := s.resolvePath(dst); derr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": derr.Error()})
			return
		}
		src, _ := fh.Open()
		if err := copyToFile(dst, src, fh.Size); err != nil {
			src.Close()
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		src.Close()
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "uploaded"})
}

func (s *Server) handleFileNew(w http.ResponseWriter, r *http.Request) {
	body, _ := readBodyLimit(r, 64<<10)
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	abs, err := s.resolvePath(req.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, err := os.Stat(abs); err == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "文件已存在"})
		return
	}
	if err := atomicWrite(abs, nil); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "created"})
}

func (s *Server) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	body, _ := readBodyLimit(r, 64<<10)
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	abs, err := s.resolvePath(req.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := os.Mkdir(abs, 0o755); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "created"})
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	abs, err := s.resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "deleted"})
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmppath := tmp.Name()
	if len(data) > 0 {
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			os.Remove(tmppath)
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmppath)
		return err
	}
	return os.Rename(tmppath, path)
}

func copyToFile(dst string, src io.Reader, size int64) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".up-*")
	if err != nil {
		return err
	}
	tmppath := tmp.Name()
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmppath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmppath)
		return err
	}
	return os.Rename(tmppath, dst)
}

func strconvQuote(s string) string { return s }
