// Dual-mode search: concurrent directory-tree traversal for millisecond-level
// name search (mode=name) and on-demand text content search (mode=content,
// skipping hidden/binary/oversized files).
package main

import (
	"bufio"
	"context"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	searchNameLimit      = 200      // max name-search results
	searchContentFiles   = 100      // max files returned by content search
	searchContentMatches = 5        // max matching lines per file
	searchContentMaxFile = 20 << 20 // skip files larger than 20 MB
	searchMaxLine        = 4 << 20  // max line length (bufio.Scanner buffer)
	searchMaxKeywords    = 20       // max number of keywords
	walkConcurrency      = 64       // concurrent walker workers
	matchTextLimit       = 200      // matching line text truncation (runes)
	dirQueueCap          = 1 << 14  // pending-directory queue capacity (~256 KB)
)

// matchLine is a single matching line in content search.
type matchLine struct {
	LineNum  int    `json:"line_num"`
	LineText string `json:"line_text"`
}

// contentResult is a single file result in content search.
type contentResult struct {
	RelPath string      `json:"rel_path"`
	Name    string      `json:"name"`
	Size    int64       `json:"size"`
	ModTime string      `json:"mod_time"`
	Matches []matchLine `json:"matches"`
}

// parseKeywords splits the query on whitespace and lowercases each keyword.
func parseKeywords(q string) []string {
	fields := strings.Fields(q)
	kws := make([]string, 0, len(fields))
	for _, f := range fields {
		kws = append(kws, strings.ToLower(f))
	}
	return kws
}

// truncateText truncates by rune so UTF-8 multi-byte characters stay intact.
func truncateText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// handleSearch is the unified entry point: parse q/mode/scope/regex, then
// dispatch to one of the two search modes.
//
// Keyword mode (default): q is split on whitespace; all keywords must match.
// Regex mode (regex=1): q is a single RE2 pattern (Go's regexp engine is
// linear-time, so arbitrary user patterns cannot hang the server). Compiled
// with an implicit (?i) prefix for case-insensitivity, matching the keyword
// mode's behavior; users can opt out locally with (?-i).
func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	if !requireGET(w, r) {
		return
	}
	q := r.URL.Query().Get("q")
	var (
		kws []string
		re  *regexp.Regexp
		err error
	)
	if boolParam(r.URL.Query().Get("regex")) {
		if strings.TrimSpace(q) == "" {
			writeAPIError(w, errBadRequest("missing search pattern (q)"))
			return
		}
		re, err = regexp.Compile("(?i)" + q)
		if err != nil {
			writeAPIError(w, errBadRequest("invalid regex: "+err.Error()))
			return
		}
	} else {
		kws = parseKeywords(q)
		if len(kws) == 0 {
			writeAPIError(w, errBadRequest("missing search keywords (q)"))
			return
		}
		if len(kws) > searchMaxKeywords {
			writeAPIError(w, errBadRequest("too many keywords (max 20)"))
			return
		}
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "name"
	}
	if mode != "name" && mode != "content" {
		writeAPIError(w, errBadRequest("invalid mode (use name or content)"))
		return
	}

	scope := a.root
	if rel := r.URL.Query().Get("scope"); rel != "" {
		var err error
		scope, err = a.resolveSafePath(rel)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		fi, err := os.Stat(scope)
		if err != nil {
			if os.IsNotExist(err) {
				writeAPIError(w, errNotFound("scope not found: "+cleanRel(rel)))
				return
			}
			writeAPIError(w, err)
			return
		}
		if !fi.IsDir() {
			writeAPIError(w, errBadRequest("scope is not a directory"))
			return
		}
	}

	if mode == "content" {
		a.searchContent(w, r, scope, kws, re)
		return
	}
	a.searchName(w, r, scope, kws, re)
}

// searchName concurrently walks the tree and matches entries whose own
// name (not the full path) contains every keyword (max 200 results).
// Matching full paths floods the results with every descendant of a
// matching directory, which users find imprecise.
//
// Directories may carry an optional free-form .whiteboard file; an entry
// matches when all keywords appear in the concatenation of its name and
// whiteboard text (or when the regex matches that text), and the whiteboard
// is included in the result.
func (a *App) searchName(w http.ResponseWriter, r *http.Request, scope string, kws []string, re *regexp.Regexp) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var (
		mu      sync.Mutex
		results = make([]item, 0, searchNameLimit)
		limited bool
	)
	_ = a.walkConcurrent(ctx, scope, false, func(abs, rel string, d fs.DirEntry, real string, info fs.FileInfo) {
		searchable := d.Name()
		var wb string
		if info.IsDir() {
			wb = readWhiteboard(abs)
			if wb != "" {
				searchable += "\n" + wb
			}
		}
		if re != nil {
			if !re.MatchString(searchable) {
				return
			}
		} else {
			low := strings.ToLower(searchable)
			for _, kw := range kws {
				if !strings.Contains(low, kw) {
					return
				}
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if len(results) >= searchNameLimit {
			limited = true
			cancel()
			return
		}
		size := info.Size()
		if info.IsDir() {
			size = 0
		}
		results = append(results, item{
			Name:       d.Name(),
			RelPath:    filepath.ToSlash(rel),
			IsDir:      info.IsDir(),
			Size:       size,
			ModTime:    info.ModTime().Format(timeLayout),
			Tags:       detectTags(d.Name()),
			Whiteboard: wb,
		})
		if len(results) >= searchNameLimit {
			limited = true
			cancel()
		}
	})
	sort.Slice(results, func(i, j int) bool { return results[i].RelPath < results[j].RelPath })
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":    "name",
		"query":   r.URL.Query().Get("q"),
		"count":   len(results),
		"limit":   searchNameLimit,
		"limited": limited,
		"results": results,
	})
}

// searchContent concurrently scans text files: skipping hidden files, binary
// files (probe contains \x00 or too many control chars) and files over 20 MB.
// A line matches when it contains every keyword (or when the regex matches);
// at most 5 lines per file and 100 files are returned.
func (a *App) searchContent(w http.ResponseWriter, r *http.Request, scope string, kws []string, re *regexp.Regexp) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var (
		mu      sync.Mutex
		results = make([]contentResult, 0)
		limited bool
	)
	_ = a.walkConcurrent(ctx, scope, true, func(abs, rel string, d fs.DirEntry, real string, info fs.FileInfo) {
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			return // skip hidden files
		}
		if info.IsDir() {
			return
		}
		if info.Size() > searchContentMaxFile {
			return // skip oversized files
		}
		f, err := os.Open(abs)
		if err != nil {
			return
		}
		defer f.Close()
		probe := make([]byte, binaryProbeBytes)
		n, _ := io.ReadFull(f, probe)
		if isBinaryData(probe[:n]) {
			return // skip binary files
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), searchMaxLine)
		var matches []matchLine
		lineNum := 0
		for sc.Scan() {
			lineNum++
			if len(matches) >= searchContentMatches {
				break
			}
			line := sc.Text()
			var ok bool
			if re != nil {
				ok = re.MatchString(line)
			} else {
				low := strings.ToLower(line)
				ok = true
				for _, kw := range kws {
					if !strings.Contains(low, kw) {
						ok = false
						break
					}
				}
			}
			if !ok {
				continue
			}
			matches = append(matches, matchLine{
				LineNum:  lineNum,
				LineText: truncateText(line, matchTextLimit),
			})
		}
		if len(matches) == 0 {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if len(results) >= searchContentFiles {
			limited = true
			cancel()
			return
		}
		results = append(results, contentResult{
			RelPath: filepath.ToSlash(rel),
			Name:    name,
			Size:    info.Size(),
			ModTime: info.ModTime().Format(timeLayout),
			Matches: matches,
		})
		if len(results) >= searchContentFiles {
			limited = true
			cancel()
		}
	})
	sort.Slice(results, func(i, j int) bool { return results[i].RelPath < results[j].RelPath })
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":    "content",
		"query":   r.URL.Query().Get("q"),
		"count":   len(results),
		"limit":   searchContentFiles,
		"limited": limited,
		"results": results,
	})
}

// visitFunc is the walker callback: abs is the entry's absolute path (via the
// symlink logical path), rel is the root-relative logical path, real is the
// symlink-resolved path, info is the FileInfo after following symlinks.
type visitFunc func(abs, rel string, d fs.DirEntry, real string, info fs.FileInfo)

// walkConcurrent traverses a directory tree concurrently:
//   - a fixed number of workers plus a bounded directory queue keep memory and
//     goroutine counts constant regardless of tree size;
//   - a visited set (keyed by real path) prevents symlink cycles;
//   - with skipHiddenDirs, hidden directories are not descended into
//     (content-search mode);
//   - symlinks are skipped entirely when not followed, and only in-root
//     targets are visited when followed.
func (a *App) walkConcurrent(ctx context.Context, start string, skipHiddenDirs bool, fn visitFunc) error {
	var (
		visited sync.Map // real path -> struct{}
		pending int64    // pending directory count
		wg      sync.WaitGroup
		dirCh   = make(chan string, dirQueueCap)
	)
	maybeDone := func() {
		if atomic.AddInt64(&pending, -1) == 0 {
			close(dirCh)
		}
	}
	var processDir func(dir string)
	processDir = func(dir string) {
		if ctx.Err() != nil {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if ctx.Err() != nil {
				return
			}
			name := e.Name()
			abs := filepath.Join(dir, name)
			rel, err := filepath.Rel(a.root, abs)
			if err != nil {
				continue
			}
			var (
				info  fs.FileInfo
				real  string
				isDir = e.IsDir()
			)
			if e.Type()&fs.ModeSymlink != 0 {
				if !a.followSymlinks {
					continue
				}
				rp, err := filepath.EvalSymlinks(abs)
				if err != nil || !withinRoot(a.root, rp) {
					continue // broken or escaping symlinks are always skipped
				}
				fi, err := os.Stat(rp)
				if err != nil {
					continue
				}
				real, info, isDir = rp, fi, fi.IsDir()
			} else {
				fi, err := e.Info()
				if err != nil {
					continue
				}
				real, info = abs, fi
			}
			fn(abs, rel, e, real, info)
			if !isDir {
				continue
			}
			if skipHiddenDirs && strings.HasPrefix(name, ".") {
				continue
			}
			if _, dup := visited.LoadOrStore(real, struct{}{}); dup {
				continue // symlink cycle or duplicate dir: visit only once
			}
			atomic.AddInt64(&pending, 1)
			select {
			case dirCh <- abs:
			case <-ctx.Done():
				maybeDone()
			default:
				// Queue full: process inline on this goroutine (depth-first;
				// recursion depth is bounded by the OS path length limit).
				maybeDone()
				processDir(abs)
			}
		}
	}

	for i := 0; i < walkConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range dirCh {
				processDir(d)
				maybeDone()
			}
		}()
	}
	visited.Store(start, struct{}{})
	atomic.AddInt64(&pending, 1)
	select {
	case dirCh <- start:
	case <-ctx.Done():
		maybeDone()
	}
	wg.Wait()
	return nil
}
