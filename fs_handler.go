// Filesystem handlers: safe path resolution, directory listing, tag detection,
// online preview, single-file download (HTTP Range support) and streaming ZIP
// packaging of directories.
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	previewTruncateSize = 512 * 1024       // 512 KB: truncate preview beyond this
	previewHeadBytes    = 128 * 1024       // head bytes read for truncated previews
	previewTailBytes    = 384 * 1024       // tail bytes read for truncated previews
	previewFullMax      = 10 * 1024 * 1024 // full-load limit: 10 MB
	binaryProbeBytes    = 512              // binary probe sample size

	whiteboardFileName = ".whiteboard" // optional free-form metadata file in a result dir
	whiteboardMaxBytes = 4096          // max whiteboard bytes surfaced to the UI

	timeLayout = "2006-01-02 15:04:05"
)

// previewSeparator marks the omitted middle part of a truncated preview
// (referenced by tests).
const previewSeparator = "\n\n========== [showmelogs] 文件过大，中间部分已省略（点击“加载完整内容”或下载原文件查看全部） ==========\n\n"

// httpError is a business error carrying an HTTP status code.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func errBadRequest(msg string) *httpError   { return &httpError{400, msg} }
func errUnauthorized(msg string) *httpError { return &httpError{401, msg} }
func errForbidden(msg string) *httpError    { return &httpError{403, msg} }
func errNotFound(msg string) *httpError     { return &httpError{404, msg} }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeAPIError writes {"error": "..."} with the proper status code.
func writeAPIError(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		writeJSON(w, he.code, map[string]string{"error": he.msg})
		return
	}
	log.Printf("internal error: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// withinRoot reports whether p equals root or lies under it (prefix check,
// which also avoids false positives like /root2).
func withinRoot(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(os.PathSeparator))
}

// resolveSafePath maps a user-supplied relative path onto a safe absolute path
// inside the root directory, blocking path traversal (../) and escaping
// symlinks (resolved targets must stay inside the root).
func (a *App) resolveSafePath(reqPath string) (string, error) {
	// 1. Strip the leading slash, then clean . and .. (/a/../b -> /b).
	p := path.Clean(strings.TrimPrefix(reqPath, "/"))
	if p == "." {
		p = ""
	}
	// 2. Reject anything that still tries to escape the root.
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", errForbidden("path escapes root directory")
	}
	full := filepath.Join(a.root, filepath.FromSlash(p))
	// 3. The logical path must equal the root or start with root + separator.
	if !withinRoot(a.root, full) {
		return "", errForbidden("path escapes root directory")
	}
	// 4. With symlinks disabled, no path component may be a symlink.
	if !a.followSymlinks {
		isLink, err := hasSymlinkComponent(a.root, p)
		if err != nil {
			if os.IsNotExist(err) {
				return full, nil
			}
			return "", err
		}
		if isLink {
			return "", errForbidden("symlink access disabled (-follow-symlinks=false)")
		}
		return full, nil
	}
	// 5. With symlinks enabled, resolve the real path and verify it stays
	//    inside the root.
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		if os.IsNotExist(err) {
			return full, nil // missing target: let the caller return 404
		}
		return "", err
	}
	if !withinRoot(a.root, real) {
		return "", errForbidden("symlink target escapes root directory")
	}
	return real, nil
}

// hasSymlinkComponent reports whether rel (relative to root) contains a
// symlink component.
func hasSymlinkComponent(root, rel string) (bool, error) {
	if rel == "" {
		return false, nil
	}
	cur := root
	for _, comp := range strings.Split(rel, "/") {
		if comp == "" || comp == "." {
			continue
		}
		cur = filepath.Join(cur, comp)
		fi, err := os.Lstat(cur)
		if err != nil {
			return false, err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return true, nil
		}
	}
	return false, nil
}

// item is the unified JSON structure for directory listings and name-search
// results.
type item struct {
	Name       string   `json:"name"`
	RelPath    string   `json:"rel_path"`
	IsDir      bool     `json:"is_dir"`
	Size       int64    `json:"size"`
	ModTime    string   `json:"mod_time"`
	Tags       []string `json:"tags"`
	Whiteboard string   `json:"whiteboard,omitempty"`
}

// detectTags derives display badges from name keywords (case-insensitive).
func detectTags(name string) []string {
	low := strings.ToLower(name)
	tags := []string{}
	if strings.Contains(low, "fail") || strings.Contains(low, ".bad") || strings.Contains(low, "error") {
		tags = append(tags, "FAIL")
	}
	if strings.Contains(low, "pass") {
		tags = append(tags, "PASS")
	}
	if strings.Contains(low, "xfstests") {
		tags = append(tags, "xfstests")
	}
	if strings.Contains(low, "ltp") {
		tags = append(tags, "LTP")
	}
	if strings.Contains(low, "fio") {
		tags = append(tags, "fio")
	}
	if strings.Contains(low, "blktests") {
		tags = append(tags, "blktests")
	}
	return tags
}

func joinRel(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

// cleanRel normalizes a user-supplied relative path for responses
// ("" means the root directory).
func cleanRel(rel string) string {
	p := path.Clean(strings.TrimPrefix(rel, "/"))
	if p == "." {
		return ""
	}
	return p
}

// handleDisk reports disk usage of the filesystem that holds the root
// directory (avail/total, like `df -h <root>` but via syscall.Statfs, so no
// external command or hardcoded path is involved).
func (a *App) handleDisk(w http.ResponseWriter, r *http.Request) {
	if !requireGET(w, r) {
		return
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(a.root, &st); err != nil {
		writeAPIError(w, err)
		return
	}
	total := int64(st.Bsize) * int64(st.Blocks)
	free := int64(st.Bsize) * int64(st.Bfree)
	avail := int64(st.Bsize) * int64(st.Bavail)
	writeJSON(w, http.StatusOK, map[string]any{
		"path":  a.root,
		"total": total,
		"free":  free,
		"avail": avail,
		"used":  total - free,
	})
}

// readWhiteboard returns the content of the optional free-form .whiteboard
// file inside dir, capped at whiteboardMaxBytes (rune-safe). It returns ""
// when the file is absent or unreadable.
func readWhiteboard(dir string) string {
	f, err := os.Open(filepath.Join(dir, whiteboardFileName))
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, whiteboardMaxBytes+1))
	if err != nil {
		return ""
	}
	if len(b) > whiteboardMaxBytes {
		return truncateText(string(b[:whiteboardMaxBytes]), whiteboardMaxBytes)
	}
	return string(b)
}

// handleList lists the direct children of the given relative path (dirs
// first, mtime descending by default).
func (a *App) handleList(w http.ResponseWriter, r *http.Request) {
	if !requireGET(w, r) {
		return
	}
	rel := r.URL.Query().Get("path")
	full, err := a.resolveSafePath(rel)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	fi, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			writeAPIError(w, errNotFound("directory not found: "+cleanRel(rel)))
			return
		}
		writeAPIError(w, err)
		return
	}
	if !fi.IsDir() {
		writeAPIError(w, errBadRequest("not a directory"))
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	relClean := cleanRel(rel)

	type raw struct {
		item item
		mod  time.Time
	}
	raws := make([]raw, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if name == whiteboardFileName {
			continue // the metadata file itself is not listed as an entry
		}
		abs := filepath.Join(full, name)
		isDir := e.IsDir()
		var info fs.FileInfo
		if e.Type()&fs.ModeSymlink != 0 {
			// Symlinks: hidden when not followed; when followed, only links
			// whose resolved target stays inside the root are shown.
			if !a.followSymlinks {
				continue
			}
			rp, err := filepath.EvalSymlinks(abs)
			if err != nil || !withinRoot(a.root, rp) {
				continue
			}
			fi2, err := os.Stat(rp)
			if err != nil {
				continue
			}
			info, isDir = fi2, fi2.IsDir()
		} else {
			fi2, err := e.Info()
			if err != nil {
				continue
			}
			info = fi2
		}
		size := info.Size()
		if isDir {
			size = 0
		}
		var wb string
		if isDir {
			wb = readWhiteboard(abs)
		}
		raws = append(raws, raw{
			item: item{
				Name:       name,
				RelPath:    joinRel(relClean, name),
				IsDir:      isDir,
				Size:       size,
				Tags:       detectTags(name),
				Whiteboard: wb,
			},
			mod: info.ModTime(),
		})
	}
	// Dirs before files; within each group sort by mtime descending, with
	// name as a stable tiebreaker.
	sort.SliceStable(raws, func(i, j int) bool {
		if raws[i].item.IsDir != raws[j].item.IsDir {
			return raws[i].item.IsDir
		}
		if !raws[i].mod.Equal(raws[j].mod) {
			return raws[i].mod.After(raws[j].mod)
		}
		return raws[i].item.Name < raws[j].item.Name
	})
	items := make([]item, 0, len(raws))
	for _, rw := range raws {
		rw.item.ModTime = rw.mod.Format(timeLayout)
		items = append(items, rw.item)
	}

	parent := ""
	if idx := strings.LastIndex(relClean, "/"); idx >= 0 {
		parent = relClean[:idx]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current_path": relClean,
		"parent_path":  parent,
		"items":        items,
	})
}

// isBinaryData reports whether the sampled bytes look binary:
//  1. containing \x00 (the SPEC rule);
//  2. heuristic addition: non-text control characters (excluding \t\n\r and
//     DEL) make up more than 5%. Pure random data can lack \x00 with ~13%
//     probability; rule 2 avoids false negatives that would render garbage
//     in the browser.
func isBinaryData(buf []byte) bool {
	if len(buf) == 0 {
		return false
	}
	if bytes.IndexByte(buf, 0) >= 0 {
		return true
	}
	ctrl := 0
	for _, b := range buf {
		if b == 0x7f || (b < 0x20 && b != '\t' && b != '\n' && b != '\r') {
			ctrl++
		}
	}
	return ctrl*20 > len(buf)
}

// isBinaryFile probes the first 512 bytes of a file for binary content.
func isBinaryFile(p string) (bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return false, err
	}
	defer f.Close()
	buf := make([]byte, binaryProbeBytes)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false, err
	}
	return isBinaryData(buf[:n]), nil
}

// readHeadTail reads the first previewHeadBytes and the last previewTailBytes
// of a file, joined by the separator line.
func readHeadTail(p string, size int64) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, previewHeadBytes)
	n, _ := io.ReadFull(f, head)
	off := size - previewTailBytes
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	tail, err := io.ReadAll(io.LimitReader(f, previewTailBytes+1))
	if err != nil {
		return "", err
	}
	return string(head[:n]) + previewSeparator + string(tail), nil
}

// handlePreview powers the split-screen reader: binary detection, truncated
// previews beyond 512 KB, full loads up to 10 MB.
func (a *App) handlePreview(w http.ResponseWriter, r *http.Request) {
	if !requireGET(w, r) {
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		writeAPIError(w, errBadRequest("missing path"))
		return
	}
	full, err := a.resolveSafePath(rel)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	fi, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			writeAPIError(w, errNotFound("file not found: "+cleanRel(rel)))
			return
		}
		writeAPIError(w, err)
		return
	}
	if !fi.Mode().IsRegular() {
		writeAPIError(w, errBadRequest("not a regular file"))
		return
	}

	relClean := cleanRel(rel)
	resp := map[string]any{
		"name":     path.Base(relClean),
		"rel_path": relClean,
		"size":     fi.Size(),
		"mod_time": fi.ModTime().Format(timeLayout),
	}

	bin, err := isBinaryFile(full)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if bin {
		resp["is_binary"] = true
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["is_binary"] = false

	fv := r.URL.Query().Get("full")
	fullReq := fv == "1" || strings.EqualFold(fv, "true") || strings.EqualFold(fv, "yes")
	size := fi.Size()

	switch {
	case fullReq && size > previewFullMax:
		// Over 10 MB: full load is unavailable, fall back to truncated preview.
		content, err := readHeadTail(full, size)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		resp["truncated"] = true
		resp["note"] = "file exceeds 10MB full-load limit"
		resp["content"] = content
	case !fullReq && size > previewTruncateSize:
		// Default: >512 KB reads first 128 KB + last 384 KB.
		content, err := readHeadTail(full, size)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		resp["truncated"] = true
		resp["content"] = content
	default:
		b, err := os.ReadFile(full)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		resp["truncated"] = false
		resp["content"] = string(b)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDownload serves single files as-is (http.ServeFile natively supports
// Range requests) and streams directories as ZIPs without temp files.
func (a *App) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !requireGET(w, r) {
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		writeAPIError(w, errBadRequest("missing path"))
		return
	}
	full, err := a.resolveSafePath(rel)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	fi, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			writeAPIError(w, errNotFound("not found: "+cleanRel(rel)))
			return
		}
		writeAPIError(w, err)
		return
	}
	if fi.IsDir() {
		a.streamZip(w, r, full)
		return
	}
	if !fi.Mode().IsRegular() {
		writeAPIError(w, errBadRequest("not a regular file"))
		return
	}
	name := path.Base(cleanRel(rel))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"`, strings.ReplaceAll(name, `"`, "_")))
	http.ServeFile(w, r, full)
}

// streamZip writes a directory to the HTTP response as a streaming ZIP.
// Symlinks and non-regular files (devices, pipes, ...) are skipped to avoid
// escapes and blocking reads.
func (a *App) streamZip(w http.ResponseWriter, r *http.Request, dir string) {
	name := filepath.Base(dir)
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "showmelogs"
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s.zip"`, strings.ReplaceAll(name, `"`, "_")))
	zw := zip.NewWriter(w)
	var walkErr error
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir // unreadable subtree: skip it
			}
			return nil
		}
		if p == dir {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // ZIP entries never include symlinks
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		zh, err := zw.CreateHeader(&zip.FileHeader{
			Name:     filepath.ToSlash(rel),
			Method:   zip.Deflate,
			Modified: info.ModTime(),
		})
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		_, err = io.Copy(zh, f)
		f.Close()
		return err
	})
	if err != nil {
		walkErr = err
	}
	if err := zw.Close(); err != nil && walkErr == nil {
		walkErr = err
	}
	if walkErr != nil {
		// Headers are already sent; log only.
		log.Printf("zip error for %s: %v", dir, walkErr)
	}
}
