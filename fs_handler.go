// Filesystem handlers: safe path resolution, directory listing, tag detection,
// online preview, single-file download (HTTP Range support) and streaming ZIP
// packaging of directories.
package main

import (
	"archive/zip"
	"bufio"
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
	"strconv"
	"strings"
	"sync"
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

	chunkDefaultBytes = 1 << 20 // default chunk size for lazy middle loading

	timeLayout = "2006-01-02 15:04:05"
)

// boolParam parses a boolean query parameter ("1", "true", "yes").
func boolParam(s string) bool {
	return s == "1" || strings.EqualFold(s, "true") || strings.EqualFold(s, "yes")
}

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

// lineIndex caches the byte offsets of every line start in a file, enabling
// O(1) line-based chunk reads and a total line count. A single entry is kept
// (keyed by path+size+mtime), so memory cost is bounded by the one file
// currently being paged through (~4-8 MB per 100 MB log, released when
// another file replaces it).
type lineIndex struct {
	path    string
	size    int64
	mtime   time.Time
	offsets []int64 // offsets[i] = byte offset of line i+1
}

var (
	lineIdxMu    sync.Mutex
	lineIdxCache *lineIndex
)

// buildLineIndex streams through the file once to record every line start.
func buildLineIndex(p string, fi os.FileInfo) (*lineIndex, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	idx := &lineIndex{path: p, size: fi.Size(), mtime: fi.ModTime()}
	idx.offsets = append(idx.offsets, 0)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), searchMaxLine)
	off := int64(0)
	for sc.Scan() {
		off += int64(len(sc.Bytes())) + 1 // +1 for the newline
		idx.offsets = append(idx.offsets, off)
	}
	// The last appended entry is the end of the final line, not a line
	// start: drop it so len(offsets) == number of lines.
	if len(idx.offsets) > 1 {
		idx.offsets = idx.offsets[:len(idx.offsets)-1]
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return idx, nil
}

// getLineIndex returns the cached index for p when still fresh (same path,
// size and mtime), otherwise builds and caches a new one.
func getLineIndex(p string, fi os.FileInfo) (*lineIndex, error) {
	lineIdxMu.Lock()
	defer lineIdxMu.Unlock()
	if lineIdxCache != nil && lineIdxCache.path == p &&
		lineIdxCache.size == fi.Size() && lineIdxCache.mtime.Equal(fi.ModTime()) {
		return lineIdxCache, nil
	}
	idx, err := buildLineIndex(p, fi)
	if err != nil {
		return nil, err
	}
	lineIdxCache = idx
	return idx, nil
}

// readChunk returns the complete lines [from, endLine] (1-based, inclusive),
// starting at the line's byte offset and covering as many lines as fit in
// maxBytes. A single line larger than maxBytes is still returned whole, up to
// searchMaxLine; beyond that it is cut (pathological inputs only).
func (idx *lineIndex) readChunk(f *os.File, from int, maxBytes int64) (content string, endLine int, more bool, err error) {
	L := len(idx.offsets)
	if from < 1 || from > L {
		return "", 0, false, fmt.Errorf("from_line %d out of range [1,%d]", from, L)
	}
	// lineEnd returns the byte offset just past line i (0-based index).
	lineEnd := func(i int) int64 {
		if i+1 < L {
			return idx.offsets[i+1]
		}
		return idx.size
	}
	start := idx.offsets[from-1]
	e := from - 1 // 0-based index of the last line to include
	for e < L-1 && lineEnd(e+1)-start <= maxBytes {
		e++
	}
	if e == from-1 && e < L-1 {
		// The first line alone exceeds maxBytes.
		if lineEnd(e)-start <= searchMaxLine {
			e++ // still fits the hard cap: include it whole
		} else {
			// Oversized single line: cut it at the hard cap.
			b, err := sliceFile(f, start, start+searchMaxLine)
			if err != nil {
				return "", 0, false, err
			}
			return string(b), from, true, nil
		}
	}
	b, err := sliceFile(f, start, lineEnd(e))
	if err != nil {
		return "", 0, false, err
	}
	return string(b), e + 1, e+1 < L, nil
}

// sliceFile reads [start, end) from f.
func sliceFile(f *os.File, start, end int64) ([]byte, error) {
	b := make([]byte, end-start)
	if _, err := f.ReadAt(b, start); err != nil && err != io.EOF {
		return nil, err
	}
	return b, nil
}

// handlePreview powers the split-screen reader: binary detection, truncated
// head+tail previews beyond 512 KB (with lazy middle chunks via from_line),
// and full loads up to 10 MB.
//
// Response shapes:
//   - small file / full load: {"truncated": false, "content": ...}
//   - big file: {"truncated": true, "head": ..., "tail": ...,
//     "head_end_line", "tail_start_line", "total_lines"} — the frontend can
//     fetch the omitted middle with from_line/max_bytes, line numbers stay
//     exact thanks to the line index.
//   - chunk: {"start_line", "end_line", "total_lines", "more", "content"}
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

	// Chunk mode: serve a line range for lazy middle loading.
	if fl := r.URL.Query().Get("from_line"); fl != "" {
		from, err := strconv.Atoi(fl)
		if err != nil || from < 1 {
			writeAPIError(w, errBadRequest("invalid from_line"))
			return
		}
		maxBytes := int64(chunkDefaultBytes)
		if mb := r.URL.Query().Get("max_bytes"); mb != "" {
			v, err := strconv.ParseInt(mb, 10, 64)
			if err != nil || v < 1024 || v > 16<<20 {
				writeAPIError(w, errBadRequest("invalid max_bytes"))
				return
			}
			maxBytes = v
		}
		idx, err := getLineIndex(full, fi)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		f, err := os.Open(full)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		defer f.Close()
		content, endLine, more, err := idx.readChunk(f, from, maxBytes)
		if err != nil {
			writeAPIError(w, errBadRequest(err.Error()))
			return
		}
		resp["start_line"] = from
		resp["end_line"] = endLine
		resp["total_lines"] = len(idx.offsets)
		resp["more"] = more
		resp["content"] = content
		writeJSON(w, http.StatusOK, resp)
		return
	}

	fullReq := boolParam(r.URL.Query().Get("full"))
	size := fi.Size()

	switch {
	case fullReq && size <= previewFullMax:
		// Full load within the 10 MB cap.
		b, err := os.ReadFile(full)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		resp["truncated"] = false
		resp["content"] = string(b)
	case !fullReq && size <= previewTruncateSize:
		// Small files are returned whole.
		b, err := os.ReadFile(full)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		resp["truncated"] = false
		resp["content"] = string(b)
	default:
		// Big file: head chunk + tail chunk with gap metadata (also the
		// fallback when a full load is requested beyond the 10 MB cap).
		idx, err := getLineIndex(full, fi)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		f, err := os.Open(full)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		defer f.Close()
		head, headEnd, _, err := idx.readChunk(f, 1, previewHeadBytes)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		// Tail: first line whose offset reaches size-previewTailBytes.
		tailStart := 1
		for i, off := range idx.offsets {
			if off >= size-previewTailBytes {
				tailStart = i + 1
				break
			}
		}
		tail, _, _, err := idx.readChunk(f, tailStart, previewTailBytes)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		resp["truncated"] = true
		resp["head"] = head
		resp["tail"] = tail
		resp["head_end_line"] = headEnd
		resp["tail_start_line"] = tailStart
		resp["total_lines"] = len(idx.offsets)
		if fullReq && size > previewFullMax {
			resp["note"] = "file exceeds 10MB full-load limit"
		}
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
