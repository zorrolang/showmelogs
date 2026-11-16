package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestApp creates an App rooted at a fresh temp dir (symlinks enabled).
func newTestApp(t *testing.T, mutate func(*App)) *App {
	t.Helper()
	root := t.TempDir()
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	app := &App{root: root, followSymlinks: true}
	if mutate != nil {
		mutate(app)
	}
	return app
}

func mustWriteFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setMtime(t *testing.T, p string, m time.Time) {
	t.Helper()
	if err := os.Chtimes(p, m, m); err != nil {
		t.Fatal(err)
	}
}

func doGet(t *testing.T, app *App, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, req)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
}

// TestResolveSafePathTraversal: path-traversal attacks must be blocked.
func TestResolveSafePathTraversal(t *testing.T) {
	app := newTestApp(t, nil)
	for _, p := range []string{
		"../../etc/passwd",
		"..",
		"../etc/passwd",
		"a/../../etc/passwd",
		"a/../../../etc",
	} {
		_, err := app.resolveSafePath(p)
		var he *httpError
		if !errors.As(err, &he) || he.code != http.StatusForbidden {
			t.Errorf("resolveSafePath(%q) err=%v, want 403", p, err)
		}
	}
	// A leading slash is stripped per spec and treated as root-relative, so
	// /etc/passwd resolves inside the root (never to the system /etc).
	got, err := app.resolveSafePath("/etc/passwd")
	if err != nil {
		t.Fatalf("resolveSafePath(/etc/passwd) unexpected error: %v", err)
	}
	want := filepath.Join(app.root, "etc", "passwd")
	if got != want {
		t.Errorf("resolveSafePath(/etc/passwd) = %q, want %q", got, want)
	}
	// Normal paths and redundant cleanup.
	for _, p := range []string{"", "a/../b", "a//b/./c", "a/b/c"} {
		if _, err := app.resolveSafePath(p); err != nil {
			t.Errorf("resolveSafePath(%q) unexpected error: %v", p, err)
		}
	}
	if got, _ := app.resolveSafePath(""); got != app.root {
		t.Errorf("empty path should resolve to root, got %q", got)
	}
}

// TestPathTraversalHTTP: traversal attacks over HTTP must return 403 JSON.
func TestPathTraversalHTTP(t *testing.T) {
	app := newTestApp(t, nil)
	for _, target := range []string{
		"/api/list?path=../../etc/passwd",
		"/api/download?path=..%2F..%2Fetc%2Fpasswd",
		"/api/preview?path=../x",
		"/api/search?q=x&mode=name&scope=../../etc",
	} {
		w := doGet(t, app, target)
		if w.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403 (body: %s)", target, w.Code, w.Body.String())
			continue
		}
		var resp struct {
			Error string `json:"error"`
		}
		decodeBody(t, w, &resp)
		if resp.Error == "" {
			t.Errorf("GET %s: missing error message in body", target)
		}
	}
}

// TestListSortingAndTags: dirs first + mtime descending + tag detection.
func TestListSortingAndTags(t *testing.T) {
	app := newTestApp(t, nil)
	base := time.Now().Add(-time.Hour)
	mustWriteFile(t, filepath.Join(app.root, "z_ltp_run", "x.log"), "x")
	mustWriteFile(t, filepath.Join(app.root, "20261117_xfstests_6.12_xfs", "check.log"), "c")
	mustWriteFile(t, filepath.Join(app.root, "results.fail.log"), "f")
	mustWriteFile(t, filepath.Join(app.root, "pass.log"), "p")
	mustWriteFile(t, filepath.Join(app.root, "blktests.out"), "b")
	mustWriteFile(t, filepath.Join(app.root, "generic.bad"), "g")
	setMtime(t, filepath.Join(app.root, "z_ltp_run"), base.Add(1*time.Minute))
	setMtime(t, filepath.Join(app.root, "20261117_xfstests_6.12_xfs"), base.Add(3*time.Minute))
	setMtime(t, filepath.Join(app.root, "results.fail.log"), base.Add(2*time.Minute))
	setMtime(t, filepath.Join(app.root, "pass.log"), base.Add(4*time.Minute))
	setMtime(t, filepath.Join(app.root, "blktests.out"), base.Add(0))
	setMtime(t, filepath.Join(app.root, "generic.bad"), base.Add(5*time.Minute))

	w := doGet(t, app, "/api/list?path=")
	if w.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		CurrentPath string `json:"current_path"`
		ParentPath  string `json:"parent_path"`
		Items       []item `json:"items"`
	}
	decodeBody(t, w, &resp)
	if resp.CurrentPath != "" || resp.ParentPath != "" {
		t.Errorf("unexpected paths: %q %q", resp.CurrentPath, resp.ParentPath)
	}
	if len(resp.Items) != 6 {
		t.Fatalf("items = %d, want 6", len(resp.Items))
	}
	// Dirs come first.
	if !resp.Items[0].IsDir || !resp.Items[1].IsDir {
		t.Errorf("first two items must be dirs: %+v %+v", resp.Items[0], resp.Items[1])
	}
	// Dirs sorted by mtime descending: 3min > 1min.
	if resp.Items[0].Name != "20261117_xfstests_6.12_xfs" || resp.Items[1].Name != "z_ltp_run" {
		t.Errorf("dir order wrong: %s, %s", resp.Items[0].Name, resp.Items[1].Name)
	}
	// Files sorted by mtime descending.
	wantFiles := []string{"generic.bad", "pass.log", "results.fail.log", "blktests.out"}
	for i, n := range wantFiles {
		if resp.Items[2+i].Name != n {
			t.Errorf("file order [%d] = %s, want %s", i, resp.Items[2+i].Name, n)
		}
	}
	tags := map[string][]string{}
	for _, it := range resp.Items {
		tags[it.Name] = it.Tags
	}
	checks := map[string][]string{
		"results.fail.log":           {"FAIL"},
		"generic.bad":                {"FAIL"},
		"pass.log":                   {"PASS"},
		"blktests.out":               {"blktests"},
		"20261117_xfstests_6.12_xfs": {"xfstests"},
		"z_ltp_run":                  {"LTP"},
	}
	for name, want := range checks {
		if fmt.Sprint(tags[name]) != fmt.Sprint(want) {
			t.Errorf("tags for %s = %v, want %v", name, tags[name], want)
		}
	}
	if resp.Items[0].RelPath != "20261117_xfstests_6.12_xfs" {
		t.Errorf("rel_path = %q", resp.Items[0].RelPath)
	}
	if w := doGet(t, app, "/api/list?path=no_such_dir"); w.Code != http.StatusNotFound {
		t.Errorf("missing dir = %d, want 404", w.Code)
	}
}

// TestSymlinkHandling: escaping links hidden/denied, in-root links browsable,
// everything denied with -follow-symlinks=false.
func TestSymlinkHandling(t *testing.T) {
	app := newTestApp(t, nil)
	mustWriteFile(t, filepath.Join(app.root, "real", "a.txt"), "hello")
	if err := os.Symlink(filepath.Join(app.root, "real"), filepath.Join(app.root, "link")); err != nil {
		t.Skip("symlink not supported")
	}
	if err := os.Symlink("/etc", filepath.Join(app.root, "evil")); err != nil {
		t.Fatal(err)
	}
	// Listing: escaping link hidden, in-root link visible.
	w := doGet(t, app, "/api/list?path=")
	var resp struct {
		Items []item `json:"items"`
	}
	decodeBody(t, w, &resp)
	names := map[string]bool{}
	for _, it := range resp.Items {
		names[it.Name] = true
	}
	if names["evil"] {
		t.Error("escaping symlink should be hidden from listing")
	}
	if !names["link"] {
		t.Error("in-root symlink should be listed")
	}
	// Direct access to an escaping link -> 403.
	if w := doGet(t, app, "/api/download?path=evil"); w.Code != http.StatusForbidden {
		t.Errorf("download evil = %d, want 403", w.Code)
	}
	// In-root links are browsable.
	w = doGet(t, app, "/api/list?path=link")
	if w.Code != http.StatusOK {
		t.Fatalf("list link = %d: %s", w.Code, w.Body.String())
	}
	var resp2 struct {
		CurrentPath string `json:"current_path"`
	}
	decodeBody(t, w, &resp2)
	if resp2.CurrentPath != "link" {
		t.Errorf("current_path = %q, want link", resp2.CurrentPath)
	}
	// -follow-symlinks=false: all symlinks hidden and access denied.
	app2 := &App{root: app.root, followSymlinks: false}
	w = doGet(t, app2, "/api/list?path=")
	names = map[string]bool{}
	decodeBody(t, w, &resp)
	for _, it := range resp.Items {
		names[it.Name] = true
	}
	if names["link"] || names["evil"] {
		t.Errorf("follow=false should hide all symlinks, got %v", names)
	}
	if w := doGet(t, app2, "/api/download?path=link"); w.Code != http.StatusForbidden {
		t.Errorf("follow=false download link = %d, want 403", w.Code)
	}
}

// TestSearchNameMode: multi-keyword name/path search.
func TestSearchNameMode(t *testing.T) {
	app := newTestApp(t, nil)
	base := filepath.Join(app.root, "20261117_xfstests_6.12_xfs")
	mustWriteFile(t, filepath.Join(base, "generic", "001_6.12_ext4.out"), "ok")
	mustWriteFile(t, filepath.Join(base, "shared", "002_6.12_xfs.out"), "ok")
	mustWriteFile(t, filepath.Join(app.root, "other.log"), "ok")

	// Multiple keywords + case insensitivity.
	cases := []struct {
		q, wantRel string
	}{
		{"6.12 ext4", "20261117_xfstests_6.12_xfs/generic/001_6.12_ext4.out"},
		{"EXT4", "20261117_xfstests_6.12_xfs/generic/001_6.12_ext4.out"},
	}
	for _, c := range cases {
		w := doGet(t, app, "/api/search?q="+url.QueryEscape(c.q)+"&mode=name")
		if w.Code != http.StatusOK {
			t.Fatalf("search %q status %d: %s", c.q, w.Code, w.Body.String())
		}
		var resp struct {
			Count   int    `json:"count"`
			Results []item `json:"results"`
		}
		decodeBody(t, w, &resp)
		if resp.Count != 1 || len(resp.Results) != 1 {
			t.Errorf("q=%q count=%d, want 1 (%+v)", c.q, resp.Count, resp.Results)
			continue
		}
		if resp.Results[0].RelPath != c.wantRel {
			t.Errorf("q=%q rel=%q, want %q", c.q, resp.Results[0].RelPath, c.wantRel)
		}
	}
	// Only the entry's own name is matched (not the full path): "6.12 xfs"
	// hits the top dir and 002_6.12_xfs.out, but not generic/shared dirs or
	// files living under a matching parent.
	w := doGet(t, app, "/api/search?q=6.12+xfs&mode=name")
	var resp struct {
		Count   int    `json:"count"`
		Results []item `json:"results"`
	}
	decodeBody(t, w, &resp)
	if resp.Count != 2 {
		t.Fatalf("q=6.12 xfs count=%d, want 2 (%+v)", resp.Count, resp.Results)
	}
	got := map[string]bool{}
	for _, r := range resp.Results {
		got[r.Name] = true
	}
	if !got["20261117_xfstests_6.12_xfs"] || !got["002_6.12_xfs.out"] {
		t.Errorf("q=6.12 xfs results = %v, want the top dir and 002_6.12_xfs.out", got)
	}
	// Single keyword "6.12" must not list descendants of the matching dir.
	w = doGet(t, app, "/api/search?q=6.12&mode=name")
	decodeBody(t, w, &resp)
	if resp.Count != 3 {
		t.Fatalf("q=6.12 count=%d, want 3 (%+v)", resp.Count, resp.Results)
	}
	for _, r := range resp.Results {
		switch r.Name {
		case "20261117_xfstests_6.12_xfs", "001_6.12_ext4.out", "002_6.12_xfs.out":
		default:
			t.Errorf("q=6.12 unexpected result %q (descendants must not match)", r.Name)
		}
	}
	// Scope limited to a subdirectory.
	w = doGet(t, app, "/api/search?q=ext4&mode=name&scope=20261117_xfstests_6.12_xfs%2Fgeneric")
	decodeBody(t, w, &resp)
	if resp.Count != 1 || len(resp.Results) != 1 {
		t.Errorf("scoped search count=%d, want 1", resp.Count)
	}
	// Empty keywords and invalid scope.
	if w := doGet(t, app, "/api/search?q=&mode=name"); w.Code != http.StatusBadRequest {
		t.Errorf("empty q = %d, want 400", w.Code)
	}
	if w := doGet(t, app, "/api/search?q=x&mode=name&scope=nope"); w.Code != http.StatusNotFound {
		t.Errorf("bad scope = %d, want 404", w.Code)
	}
	if w := doGet(t, app, "/api/search?q=x&mode=bad"); w.Code != http.StatusBadRequest {
		t.Errorf("bad mode = %d, want 400", w.Code)
	}
	// No matches.
	w = doGet(t, app, "/api/search?q=zzz_no_match&mode=name")
	decodeBody(t, w, &resp)
	if resp.Count != 0 {
		t.Errorf("no-match count=%d, want 0", resp.Count)
	}
	// Result cap of 200: create 250 matching files.
	for i := 0; i < 250; i++ {
		mustWriteFile(t, filepath.Join(app.root, "many", fmt.Sprintf("needle_%03d.log", i)), "x")
	}
	w = doGet(t, app, "/api/search?q=needle&mode=name")
	var limited struct {
		Count   int  `json:"count"`
		Limited bool `json:"limited"`
	}
	decodeBody(t, w, &limited)
	if limited.Count != searchNameLimit || !limited.Limited {
		t.Errorf("limit: count=%d limited=%v, want %d/true", limited.Count, limited.Limited, searchNameLimit)
	}
}

// TestSearchContentMode: line numbers, text extraction, per-line multi-keyword
// matching, skipping binary/hidden/oversized files, 5 matches per file max.
func TestSearchContentMode(t *testing.T) {
	app := newTestApp(t, nil)
	logDir := filepath.Join(app.root, "logs")
	debug := "line1: starting test\n" +
		"line2: BUG: unable to handle kernel NULL pointer\n" +
		"line3: Call Trace:\n" +
		"line4: ? ext4_do_something\n" +
		"line5: end of test\n" +
		"line6: BUG: another oops with Call Trace\n" +
		"line7: BUG: " + strings.Repeat("x", 500) + "\n"
	mustWriteFile(t, filepath.Join(logDir, "debug.log"), debug)
	// Binary file: must be skipped.
	mustWriteFile(t, filepath.Join(logDir, "binary.dat"), "AB\x00CD BUG: in binary\n")
	// Hidden file: must be skipped.
	mustWriteFile(t, filepath.Join(logDir, ".hidden.log"), "BUG: hidden file\n")
	// Hidden directory: must be skipped.
	mustWriteFile(t, filepath.Join(logDir, ".git", "objects", "ab"), "BUG: in hidden dir\n")
	// 21 MB oversized file: must be skipped.
	huge := filepath.Join(logDir, "huge.log")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(21 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// Max 5 matches per file.
	var multi strings.Builder
	for i := 0; i < 8; i++ {
		fmt.Fprintf(&multi, "multi BUG line %d\n", i)
	}
	mustWriteFile(t, filepath.Join(logDir, "multi.log"), multi.String())

	type fileRes struct {
		RelPath string      `json:"rel_path"`
		Matches []matchLine `json:"matches"`
	}
	var resp struct {
		Count   int       `json:"count"`
		Results []fileRes `json:"results"`
	}

	w := doGet(t, app, "/api/search?q=BUG&mode=content")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	decodeBody(t, w, &resp)
	if resp.Count != 2 || len(resp.Results) != 2 {
		t.Fatalf("count=%d, want 2 (debug.log + multi.log), got %+v", resp.Count, resp.Results)
	}
	if resp.Results[0].RelPath != "logs/debug.log" || resp.Results[1].RelPath != "logs/multi.log" {
		t.Errorf("results = %s, %s", resp.Results[0].RelPath, resp.Results[1].RelPath)
	}
	r := resp.Results[0]
	if len(r.Matches) != 3 {
		t.Fatalf("debug.log matches=%d, want 3", len(r.Matches))
	}
	if r.Matches[0].LineNum != 2 || r.Matches[1].LineNum != 6 || r.Matches[2].LineNum != 7 {
		t.Errorf("line nums = %d %d %d, want 2 6 7", r.Matches[0].LineNum, r.Matches[1].LineNum, r.Matches[2].LineNum)
	}
	if !strings.Contains(r.Matches[0].LineText, "BUG:") {
		t.Errorf("line text mismatch: %q", r.Matches[0].LineText)
	}
	// Overlong lines are rune-truncated to 200 chars plus an ellipsis.
	if n := len([]rune(r.Matches[2].LineText)); n != 201 {
		t.Errorf("truncated line runes = %d, want 201", n)
	}
	if !strings.HasSuffix(r.Matches[2].LineText, "…") {
		t.Errorf("truncated line should end with ellipsis: %q", r.Matches[2].LineText)
	}
	// Max 5 matches per file.
	if n := len(resp.Results[1].Matches); n != 5 {
		t.Errorf("multi.log matches=%d, want 5", n)
	}

	// All keywords must appear on the same line: BUG + Call + Trace hits
	// only line 6.
	w = doGet(t, app, "/api/search?q=BUG+Call+Trace&mode=content")
	decodeBody(t, w, &resp)
	if resp.Count != 1 || len(resp.Results[0].Matches) != 1 || resp.Results[0].Matches[0].LineNum != 6 {
		t.Errorf("multi-keyword: count=%d matches=%+v, want 1 file / line 6",
			resp.Count, resp.Results)
	}
	// Case insensitivity.
	w = doGet(t, app, "/api/search?q=call+trace&mode=content")
	decodeBody(t, w, &resp)
	if resp.Count != 1 || resp.Results[0].Matches[0].LineNum != 3 {
		t.Errorf("case-insensitive: count=%d, want 1 (line 3)", resp.Count)
	}
}

// TestPreviewTruncation: >512KB files get head+tail previews; full loads work
// up to 10 MB.
func TestPreviewTruncation(t *testing.T) {
	app := newTestApp(t, nil)
	unit := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ\n"
	const size600K = 600 * 1024
	content := strings.Repeat(unit, size600K/len(unit)+1)[:size600K]
	mustWriteFile(t, filepath.Join(app.root, "big.log"), content)

	type prev struct {
		IsBinary  bool   `json:"is_binary"`
		Truncated bool   `json:"truncated"`
		Content   string `json:"content"`
		Size      int64  `json:"size"`
		Note      string `json:"note"`
	}
	var p prev

	// Default: truncated preview (first 128KB + separator + last 384KB).
	w := doGet(t, app, "/api/preview?path=big.log")
	if w.Code != http.StatusOK {
		t.Fatalf("preview status %d", w.Code)
	}
	decodeBody(t, w, &p)
	if p.IsBinary {
		t.Fatal("big.log should not be binary")
	}
	if !p.Truncated {
		t.Fatal("want truncated")
	}
	if p.Size != size600K {
		t.Errorf("size = %d, want %d", p.Size, size600K)
	}
	wantLen := previewHeadBytes + len(previewSeparator) + previewTailBytes
	if len(p.Content) != wantLen {
		t.Errorf("content len = %d, want %d", len(p.Content), wantLen)
	}
	if !strings.HasPrefix(p.Content, content[:previewHeadBytes]) {
		t.Error("head mismatch")
	}
	if !strings.HasSuffix(p.Content, content[size600K-previewTailBytes:]) {
		t.Error("tail mismatch")
	}
	if !strings.Contains(p.Content, previewSeparator) {
		t.Error("missing separator")
	}

	// full=true: 600KB < 10MB, so the complete content is returned.
	w = doGet(t, app, "/api/preview?path=big.log&full=1")
	decodeBody(t, w, &p)
	if p.Truncated {
		t.Error("full: should not truncate 600KB file")
	}
	if p.Content != content {
		t.Error("full: content mismatch")
	}

	// Small files (<=512KB) are returned whole.
	small := "hello log\nsecond line\n"
	mustWriteFile(t, filepath.Join(app.root, "small.txt"), small)
	w = doGet(t, app, "/api/preview?path=small.txt")
	decodeBody(t, w, &p)
	if p.Truncated || p.Content != small {
		t.Errorf("small file: truncated=%v content=%q", p.Truncated, p.Content)
	}

	// 11MB file with full=true: over the 10MB limit, still truncated with a
	// note.
	const size11M = 11 * 1024 * 1024
	big := strings.Repeat(unit, size11M/len(unit)+1)[:size11M]
	mustWriteFile(t, filepath.Join(app.root, "huge.txt"), big)
	w = doGet(t, app, "/api/preview?path=huge.txt&full=1")
	decodeBody(t, w, &p)
	if !p.Truncated {
		t.Error("11MB full: want truncated")
	}
	if p.Note == "" {
		t.Error("11MB full: want note about limit")
	}
}

// TestPreviewBinary: binary files are flagged and return no content.
func TestPreviewBinary(t *testing.T) {
	app := newTestApp(t, nil)
	mustWriteFile(t, filepath.Join(app.root, "bin.dat"), "hello\x00world")
	// Pseudo-random data without \x00: caught by the control-character
	// heuristic (otherwise ~13% of random files would slip through).
	randish := make([]byte, 1024)
	for i := range randish {
		randish[i] = byte(i%255 + 1)
	}
	mustWriteFile(t, filepath.Join(app.root, "nozero.bin"), string(randish))
	for _, name := range []string{"bin.dat", "nozero.bin"} {
		w := doGet(t, app, "/api/preview?path="+name)
		if w.Code != http.StatusOK {
			t.Fatalf("preview %s status %d", name, w.Code)
		}
		var p struct {
			IsBinary bool   `json:"is_binary"`
			Content  string `json:"content"`
		}
		decodeBody(t, w, &p)
		if !p.IsBinary {
			t.Errorf("%s: want is_binary=true", name)
		}
		if p.Content != "" {
			t.Errorf("%s: binary should not include content", name)
		}
	}
}

// TestZipDownload: streaming ZIP of a directory, verified by unpacking.
func TestZipDownload(t *testing.T) {
	app := newTestApp(t, nil)
	pack := filepath.Join(app.root, "pack")
	mustWriteFile(t, filepath.Join(pack, "a.txt"), "alpha\n")
	bLog := strings.Repeat("log line\n", 1000)
	mustWriteFile(t, filepath.Join(pack, "sub", "b.log"), bLog)
	mustWriteFile(t, filepath.Join(pack, "deep", "c", "d.txt"), "delta")
	if err := os.Symlink(filepath.Join(pack, "a.txt"), filepath.Join(pack, "ln")); err != nil {
		t.Skip("symlink not supported")
	}

	w := doGet(t, app, "/api/download?path=pack")
	if w.Code != http.StatusOK {
		t.Fatalf("zip status %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("content-type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "pack.zip") {
		t.Errorf("content-disposition = %q", cd)
	}
	body := w.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("invalid zip: %v", err)
	}
	got := map[string]string{}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatalf("open %s: %v", zf.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		got[zf.Name] = string(b)
	}
	want := map[string]string{
		"a.txt":        "alpha\n",
		"sub/b.log":    bLog,
		"deep/c/d.txt": "delta",
	}
	if len(got) != len(want) {
		t.Fatalf("zip entries = %v, want %v (symlink must be skipped)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("zip entry %s content mismatch", k)
		}
	}

	// Single-file download.
	w = doGet(t, app, "/api/download?path=pack/a.txt")
	if w.Code != http.StatusOK || w.Body.String() != "alpha\n" {
		t.Errorf("file download = %d %q", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "a.txt") {
		t.Errorf("file content-disposition = %q", cd)
	}
	// Missing path -> 404.
	if w := doGet(t, app, "/api/download?path=missing"); w.Code != http.StatusNotFound {
		t.Errorf("missing download = %d, want 404", w.Code)
	}
}

// TestBasicAuth: auth enforcement with and without a configured password.
func TestBasicAuth(t *testing.T) {
	app := newTestApp(t, func(a *App) { a.user = "admin"; a.pass = "secret" })
	// No credentials -> 401 + WWW-Authenticate.
	w := doGet(t, app, "/")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), "Basic") {
		t.Error("missing WWW-Authenticate header")
	}
	// Valid credentials -> 200 and the page is served.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "secret")
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid auth = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "showmelogs") {
		t.Error("index should contain showmelogs title")
	}
	// Wrong password -> 401 (APIs are protected too).
	req = httptest.NewRequest(http.MethodGet, "/api/list?path=", nil)
	req.SetBasicAuth("admin", "wrong")
	rec = httptest.NewRecorder()
	app.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad pass = %d, want 401", rec.Code)
	}
	// No -pass configured -> open access.
	app2 := newTestApp(t, nil)
	if w := doGet(t, app2, "/api/list?path="); w.Code != http.StatusOK {
		t.Errorf("no-pass list = %d, want 200", w.Code)
	}
	if w := doGet(t, app2, "/"); w.Code != http.StatusOK {
		t.Errorf("no-pass index = %d, want 200", w.Code)
	}
}

// TestWalkSymlinkCycleSafe: symlink cycles must not hang the concurrent walk.
func TestWalkSymlinkCycleSafe(t *testing.T) {
	app := newTestApp(t, nil)
	dir := filepath.Join(app.root, "a")
	mustWriteFile(t, filepath.Join(dir, "x.log"), "x")
	if err := os.Symlink(dir, filepath.Join(dir, "loop")); err != nil {
		t.Skip("symlink not supported")
	}
	ch := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/api/search?q=x&mode=name", nil)
		w := httptest.NewRecorder()
		app.routes().ServeHTTP(w, req)
		ch <- w
	}()
	select {
	case w := <-ch:
		if w.Code != http.StatusOK {
			t.Fatalf("search status %d", w.Code)
		}
		var resp struct {
			Count int `json:"count"`
		}
		decodeBody(t, w, &resp)
		if resp.Count != 1 {
			t.Errorf("count = %d, want 1 (no loop duplication)", resp.Count)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("search hung on symlink cycle")
	}
}

// TestWhiteboard: optional free-form .whiteboard files in result dirs are
// surfaced in listings (but not listed as entries themselves), searchable in
// name mode, and capped at 4 KB.
func TestWhiteboard(t *testing.T) {
	app := newTestApp(t, nil)
	wb := "kernel 6.12 rmapbt=1 localio=no\nnotes: first round"
	mustWriteFile(t, filepath.Join(app.root, "run1", ".whiteboard"), wb)
	mustWriteFile(t, filepath.Join(app.root, "run1", "check.log"), "log")
	mustWriteFile(t, filepath.Join(app.root, "plain", "f.txt"), "x")
	// Overlong whiteboard: capped at whiteboardMaxBytes.
	mustWriteFile(t, filepath.Join(app.root, "long", ".whiteboard"), strings.Repeat("x", 8000))
	mustWriteFile(t, filepath.Join(app.root, "long", "g.txt"), "x")

	// Listing: whiteboard attached to dirs; the file itself is not an entry.
	w := doGet(t, app, "/api/list?path=")
	if w.Code != http.StatusOK {
		t.Fatalf("list status %d", w.Code)
	}
	var resp struct {
		Items []item `json:"items"`
	}
	decodeBody(t, w, &resp)
	byName := map[string]item{}
	for _, it := range resp.Items {
		byName[it.Name] = it
		if it.Name == whiteboardFileName {
			t.Error(".whiteboard must not appear as a listing entry")
		}
	}
	if got := byName["run1"].Whiteboard; got != wb {
		t.Errorf("run1 whiteboard = %q, want %q", got, wb)
	}
	if got := byName["plain"].Whiteboard; got != "" {
		t.Errorf("plain whiteboard = %q, want empty", got)
	}
	if got := byName["long"].Whiteboard; len(got) != whiteboardMaxBytes {
		t.Errorf("long whiteboard len = %d, want %d", len(got), whiteboardMaxBytes)
	}

	// Name-mode search matches whiteboard content.
	w = doGet(t, app, "/api/search?q="+url.QueryEscape("rmapbt=1")+"&mode=name")
	var sresp struct {
		Count   int    `json:"count"`
		Results []item `json:"results"`
	}
	decodeBody(t, w, &sresp)
	if sresp.Count != 1 || len(sresp.Results) != 1 {
		t.Fatalf("whiteboard search count=%d, want 1 (%+v)", sresp.Count, sresp.Results)
	}
	r := sresp.Results[0]
	if r.Name != "run1" || !strings.Contains(r.Whiteboard, "rmapbt=1") {
		t.Errorf("result = %+v, want run1 with whiteboard content", r)
	}
	// Keywords may span the name and the whiteboard together.
	w = doGet(t, app, "/api/search?q=run1+localio&mode=name")
	decodeBody(t, w, &sresp)
	if sresp.Count != 1 {
		t.Errorf("combined name+whiteboard search count=%d, want 1", sresp.Count)
	}
}

// TestSearchRegex: regex=1 enables RE2 patterns for both name and content
// search (case-insensitive by default).
func TestSearchRegex(t *testing.T) {
	app := newTestApp(t, nil)
	mustWriteFile(t, filepath.Join(app.root, "run1", ".whiteboard"), "kernel 6.12 rmapbt=1 localio=no")
	mustWriteFile(t, filepath.Join(app.root, "run1", "check.log"), "log")
	mustWriteFile(t, filepath.Join(app.root, "logs", "debug.log"),
		"line1\nBUG: NULL pointer in ext4\nBUG: another pointer\nend\n")

	type respT struct {
		Count   int    `json:"count"`
		Results []item `json:"results"`
	}
	var resp respT

	// Name mode: regex anchored to the entry name.
	w := doGet(t, app, "/api/search?q=%5Erun1&mode=name&regex=1")
	decodeBody(t, w, &resp)
	if resp.Count != 1 || resp.Results[0].Name != "run1" {
		t.Fatalf("^run1: count=%d, want 1 (run1)", resp.Count)
	}
	// Regex hitting the whiteboard only.
	w = doGet(t, app, "/api/search?q=rmapbt%3D%5Cd&mode=name&regex=1")
	decodeBody(t, w, &resp)
	if resp.Count != 1 || resp.Results[0].Name != "run1" {
		t.Fatalf("rmapbt=\\d: count=%d, want 1 (run1)", resp.Count)
	}
	// Case-insensitive by default.
	w = doGet(t, app, "/api/search?q=KERNEL+6%5C.12&mode=name&regex=1")
	decodeBody(t, w, &resp)
	if resp.Count != 1 {
		t.Fatalf("case-insensitive regex: count=%d, want 1", resp.Count)
	}
	// Invalid regex -> 400 with a compile error message.
	w = doGet(t, app, "/api/search?q=%28&mode=name&regex=1")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid regex status %d, want 400", w.Code)
	}
	var errResp struct {
		Error string `json:"error"`
	}
	decodeBody(t, w, &errResp)
	if !strings.Contains(errResp.Error, "invalid regex") {
		t.Errorf("error = %q, want invalid regex message", errResp.Error)
	}

	// Content mode: regex per line.
	type cresp struct {
		Count   int `json:"count"`
		Results []struct {
			RelPath string      `json:"rel_path"`
			Matches []matchLine `json:"matches"`
		} `json:"results"`
	}
	var cr cresp
	w = doGet(t, app, "/api/search?q=BUG%3A.*pointer&mode=content&regex=1")
	if w.Code != http.StatusOK {
		t.Fatalf("content regex status %d: %s", w.Code, w.Body.String())
	}
	decodeBody(t, w, &cr)
	if cr.Count != 1 || len(cr.Results[0].Matches) != 2 {
		t.Fatalf("content regex count=%d matches=%+v, want 1 file / 2 lines", cr.Count, cr.Results)
	}
	if cr.Results[0].Matches[0].LineNum != 2 || cr.Results[0].Matches[1].LineNum != 3 {
		t.Errorf("line nums = %d %d, want 2 3",
			cr.Results[0].Matches[0].LineNum, cr.Results[0].Matches[1].LineNum)
	}
}

// TestDiskUsage: /api/disk reports the usage of the filesystem holding the
// root directory.
func TestDiskUsage(t *testing.T) {
	app := newTestApp(t, nil)
	w := doGet(t, app, "/api/disk")
	if w.Code != http.StatusOK {
		t.Fatalf("disk status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Path  string `json:"path"`
		Total int64  `json:"total"`
		Free  int64  `json:"free"`
		Avail int64  `json:"avail"`
		Used  int64  `json:"used"`
	}
	decodeBody(t, w, &resp)
	if resp.Path != app.root {
		t.Errorf("path = %q, want %q", resp.Path, app.root)
	}
	if resp.Total <= 0 || resp.Avail <= 0 {
		t.Errorf("total=%d avail=%d, want positive values", resp.Total, resp.Avail)
	}
	if resp.Avail > resp.Free {
		t.Errorf("avail %d > free %d", resp.Avail, resp.Free)
	}
	if resp.Used != resp.Total-resp.Free {
		t.Errorf("used = %d, want %d", resp.Used, resp.Total-resp.Free)
	}
}
