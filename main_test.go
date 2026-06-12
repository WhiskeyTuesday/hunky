package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseHunkHeader(t *testing.T) {
	cases := []struct {
		header             string
		os_, oc_, ns_, nc_ int
	}{
		{"@@ -56,7 +56,7 @@ pub const Runtime = struct {", 56, 7, 56, 7},
		{"@@ -5 +5,2 @@", 5, 1, 5, 2},     // single-line old range → implicit count 1
		{"@@ -10,3 +10 @@", 10, 3, 10, 1}, // single-line new range
		{"@@ -0,0 +1,3 @@", 0, 0, 1, 3},   // new file
		{"@@ -1,3 +0,0 @@", 1, 3, 0, 0},   // deleted file
	}
	for _, c := range cases {
		os_, oc_, ns_, nc_ := parseHunkHeader(c.header)
		if os_ != c.os_ || oc_ != c.oc_ || ns_ != c.ns_ || nc_ != c.nc_ {
			t.Errorf("parseHunkHeader(%q) = %d,%d,%d,%d; want %d,%d,%d,%d",
				c.header, os_, oc_, ns_, nc_, c.os_, c.oc_, c.ns_, c.nc_)
		}
	}
}

func TestParseRange(t *testing.T) {
	good := map[string][2]int{"5-10": {5, 10}, "5-5": {5, 5}, "1-999": {1, 999}}
	for s, want := range good {
		start, end, ok := parseRange(s)
		if !ok || start != want[0] || end != want[1] {
			t.Errorf("parseRange(%q) = %d,%d,%v; want %d,%d,true", s, start, end, ok, want[0], want[1])
		}
	}
	for _, s := range []string{"10-5", "5", "a-b", "", "5-"} {
		if _, _, ok := parseRange(s); ok {
			t.Errorf("parseRange(%q) should have failed", s)
		}
	}
}

func TestPathHelpers(t *testing.T) {
	if p := pathFromDiffGit("diff --git a/src/foo.go b/src/foo.go"); p != "src/foo.go" {
		t.Errorf("pathFromDiffGit = %q", p)
	}
	if p := pathFromPlusLine("+++ b/src/foo.go"); p != "src/foo.go" {
		t.Errorf("pathFromPlusLine = %q", p)
	}
	if p := pathFromPlusLine("+++ /dev/null"); p != "" {
		t.Errorf("pathFromPlusLine(/dev/null) = %q, want empty", p)
	}
	if !pathMatches("src/foo.go", "foo.go") || !pathMatches("src/foo.go", "src/foo.go") || !pathMatches("src/foo.go", "./src/foo.go") {
		t.Error("expected match")
	}
	if pathMatches("src/foo.go", "bar.go") {
		t.Error("unexpected match")
	}
}

const twoFileDiff = `diff --git a/a.txt b/a.txt
index 1111111..2222222 100644
--- a/a.txt
+++ b/a.txt
@@ -1,2 +1,2 @@
 keep
-old a
+new a
diff --git a/b.txt b/b.txt
index 3333333..4444444 100644
--- a/b.txt
+++ b/b.txt
@@ -1 +1,2 @@
 keep b
+added b
`

func TestParseDiffMultiFile(t *testing.T) {
	files, err := parseDiff(strings.NewReader(twoFileDiff))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	if files[0].path != "a.txt" || files[1].path != "b.txt" {
		t.Errorf("paths = %q, %q", files[0].path, files[1].path)
	}
	if len(files[0].hunks) != 1 || len(files[1].hunks) != 1 {
		t.Errorf("hunk counts = %d, %d; want 1, 1", len(files[0].hunks), len(files[1].hunks))
	}
}

func TestSelectFile(t *testing.T) {
	files, _ := parseDiff(strings.NewReader(twoFileDiff))

	fd, err := selectFile(files, "b.txt")
	if err != nil || fd == nil || fd.path != "b.txt" {
		t.Errorf("selectFile(b.txt) = %v, %v", fd, err)
	}
	if _, err := selectFile(files, "nope.txt"); err == nil {
		t.Error("selectFile(nope.txt) should error (no match)")
	}
	// A single-file diff is used as-is regardless of the arg.
	single, _ := parseDiff(strings.NewReader(twoFileDiff[:strings.Index(twoFileDiff, "diff --git a/b.txt")]))
	if fd, err := selectFile(single, "whatever"); err != nil || fd == nil {
		t.Errorf("single-file selectFile = %v, %v", fd, err)
	}
}

func TestFilterHunk_dropsAndKeepsAdditions(t *testing.T) {
	// 1 context line (new 1), then 3 additions (new 2,3,4).
	h := Hunk{header: "@@ -1,1 +1,4 @@", oldStart: 1, oldCount: 1, newStart: 1, newCount: 4,
		lines: []string{" ctx", "+a2", "+a3", "+a4"}}

	out, kept := filterHunk(h, 3, 4) // keep new-lines 3 and 4 only
	if kept != 2 {
		t.Fatalf("kept = %d, want 2", kept)
	}
	if strings.Contains(out, "+a2") {
		t.Error("a2 (out of range) should be dropped")
	}
	if !strings.Contains(out, "+a3") || !strings.Contains(out, "+a4") {
		t.Error("a3/a4 (in range) should be kept")
	}
	// oldCount = context(1); newCount = context(1) + kept additions(2) = 3.
	if got := strings.SplitN(out, "\n", 2)[0]; got != "@@ -1,1 +1,3 @@" {
		t.Errorf("recomputed header = %q, want @@ -1,1 +1,3 @@", got)
	}
}

func TestFilterHunk_demotesOutOfRangeDeletion(t *testing.T) {
	// ctx1(new1), -del(new-pos2), ctx2(new2), ctx3(new3), +add(new4).
	h := Hunk{header: "@@ -1,4 +1,4 @@", oldStart: 1, oldCount: 4, newStart: 1, newCount: 4,
		lines: []string{" ctx1", "-del", " ctx2", " ctx3", "+add"}}

	out, kept := filterHunk(h, 4, 4) // only the addition is in range
	if kept != 1 {
		t.Fatalf("kept = %d, want 1", kept)
	}
	if !strings.Contains(out, "\n+add\n") {
		t.Error("the addition should be staged")
	}
	if strings.Contains(out, "-del") {
		t.Error("out-of-range deletion should be demoted to context, not staged as a removal")
	}
	if !strings.Contains(out, " del") {
		t.Error("the demoted deletion should appear as a context line")
	}
}

func TestInvertSelection(t *testing.T) {
	cases := []struct {
		selected []int
		n        int
		want     []int
	}{
		{[]int{1}, 4, []int{0, 2, 3}},
		{[]int{0, 2}, 3, []int{1}},
		{nil, 3, []int{0, 1, 2}},   // invert nothing → everything
		{[]int{0, 1, 2}, 3, nil},   // invert everything → nothing
		{[]int{2, 0}, 3, []int{1}}, // order of input doesn't matter
	}
	for _, c := range cases {
		got := invertSelection(c.selected, c.n)
		if len(got) != len(c.want) {
			t.Errorf("invertSelection(%v, %d) = %v, want %v", c.selected, c.n, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("invertSelection(%v, %d) = %v, want %v", c.selected, c.n, got, c.want)
				break
			}
		}
	}
}

func TestParseDiff_modeOnly(t *testing.T) {
	// A chmod with no content change: headers + old/new mode, no @@ hunks.
	// parseDiff must yield one file with zero hunks (the tool reports nothing
	// to stage by number; --count prints 0).
	mode := "diff --git a/run.sh b/run.sh\n" +
		"old mode 100644\n" +
		"new mode 100755\n"
	files, err := parseDiff(strings.NewReader(mode))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if len(files[0].hunks) != 0 {
		t.Errorf("mode-only change should have 0 hunks, got %d", len(files[0].hunks))
	}
	if files[0].path != "run.sh" {
		t.Errorf("path = %q", files[0].path)
	}
}

// --- integration round-trip helpers (real git in a throwaway repo) ---

type testRepo struct {
	t   *testing.T
	dir string
}

func newTestRepo(t *testing.T) *testRepo {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	r := &testRepo{t: t, dir: t.TempDir()}
	t.Chdir(r.dir)
	r.git("init", "-q")
	r.git("config", "user.email", "t@t")
	r.git("config", "user.name", "t")
	return r
}

func (r *testRepo) git(args ...string) string {
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (r *testRepo) write(name, content string) {
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// stageHunks runs the tool's pipeline (git diff → parse → build patch → apply)
// for the given file and hunk indices, the same path main() takes.
func (r *testRepo) stageHunks(file string, idxs ...int) {
	diff, err := gitDiff(file)
	if err != nil {
		r.t.Fatal(err)
	}
	files, err := parseDiff(strings.NewReader(string(diff)))
	if err != nil {
		r.t.Fatal(err)
	}
	fd, err := selectFile(files, file)
	if err != nil || fd == nil {
		r.t.Fatalf("selectFile(%s) = %v, %v", file, fd, err)
	}
	if err := applyPatch(buildPatch(fd, idxs)); err != nil {
		r.t.Fatalf("apply: %v", err)
	}
}

// TestRoundTrip: staging one of two hunks must land exactly that change in the
// index and leave the other unstaged.
func TestRoundTrip(t *testing.T) {
	r := newTestRepo(t)

	// 20 lines; change line 2 and line 18 — far enough apart (>2× context) that
	// git emits two separate hunks.
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "l" + strconv.Itoa(i+1)
	}
	r.write("f.txt", strings.Join(lines, "\n")+"\n")
	r.git("add", "f.txt")
	r.git("commit", "-q", "-m", "base")
	lines[1], lines[17] = "CHANGED2", "CHANGED18"
	r.write("f.txt", strings.Join(lines, "\n")+"\n")

	r.stageHunks("f.txt", 0) // first hunk only
	staged := r.git("diff", "--cached")
	if !strings.Contains(staged, "+CHANGED2") {
		t.Error("hunk 0 (CHANGED2) should be staged")
	}
	if strings.Contains(staged, "+CHANGED18") {
		t.Error("hunk 1 (CHANGED18) should NOT be staged")
	}
}

// TestRoundTrip_invert: --invert of hunk 0 must stage the OTHER hunk and leave
// hunk 0 unstaged — the complement of an explicit selection.
func TestRoundTrip_invert(t *testing.T) {
	r := newTestRepo(t)

	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "l" + strconv.Itoa(i+1)
	}
	r.write("f.txt", strings.Join(lines, "\n")+"\n")
	r.git("add", "f.txt")
	r.git("commit", "-q", "-m", "base")
	lines[1], lines[17] = "CHANGED2", "CHANGED18"
	r.write("f.txt", strings.Join(lines, "\n")+"\n")

	// Select hunk 0, invert → stage hunk 1 only.
	r.stageHunks("f.txt", invertSelection([]int{0}, 2)...)
	staged := r.git("diff", "--cached")
	if !strings.Contains(staged, "+CHANGED18") {
		t.Error("inverted selection should stage hunk 1 (CHANGED18)")
	}
	if strings.Contains(staged, "+CHANGED2") {
		t.Error("inverted selection should NOT stage hunk 0 (CHANGED2)")
	}
}

func TestRoundTrip_newFile(t *testing.T) {
	r := newTestRepo(t)
	r.write("base.txt", "x\n")
	r.git("add", "base.txt")
	r.git("commit", "-q", "-m", "init")

	r.write("new.txt", "hello\nworld\n")
	r.git("add", "-N", "new.txt") // intent-to-add → appears in `git diff HEAD` as a new file

	r.stageHunks("new.txt", 0)
	staged := r.git("diff", "--cached")
	if !strings.Contains(staged, "new file") || !strings.Contains(staged, "+hello") {
		t.Errorf("new file should be staged:\n%s", staged)
	}
}

func TestRoundTrip_deletedFile(t *testing.T) {
	r := newTestRepo(t)
	r.write("gone.txt", "a\nb\nc\n")
	r.git("add", "gone.txt")
	r.git("commit", "-q", "-m", "init")

	if err := os.Remove(filepath.Join(r.dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	r.stageHunks("gone.txt", 0)
	staged := r.git("diff", "--cached")
	if !strings.Contains(staged, "deleted file") {
		t.Errorf("deletion should be staged:\n%s", staged)
	}
}

func TestRoundTrip_noNewlineAtEOF(t *testing.T) {
	r := newTestRepo(t)
	r.write("nn.txt", "line1\nline2") // no trailing newline
	r.git("add", "nn.txt")
	r.git("commit", "-q", "-m", "init")
	r.write("nn.txt", "line1\nCHANGED") // still no trailing newline

	r.stageHunks("nn.txt", 0)
	staged := r.git("diff", "--cached")
	if !strings.Contains(staged, "+CHANGED") {
		t.Errorf("no-newline-at-EOF change should be staged:\n%s", staged)
	}
	if !strings.Contains(staged, "No newline at end of file") {
		t.Error("the \\ No newline marker should be preserved in the staged patch")
	}
}

func TestParseDiff_empty(t *testing.T) {
	files, err := parseDiff(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("empty diff → %d files, want 0", len(files))
	}
	if fd, err := selectFile(files, "anything"); fd != nil || err != nil {
		t.Errorf("selectFile on empty = %v, %v; want nil, nil", fd, err)
	}
}

func TestParseDiff_binary(t *testing.T) {
	// A binary diff has no @@ hunks — just headers and a "Binary files … differ"
	// line. parseDiff must yield one file with zero hunks (the tool then reports
	// nothing to stage rather than crashing).
	bin := "diff --git a/img.png b/img.png\n" +
		"index 1111111..2222222 100644\n" +
		"Binary files a/img.png and b/img.png differ\n"
	files, err := parseDiff(strings.NewReader(bin))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if len(files[0].hunks) != 0 {
		t.Errorf("binary file should have 0 hunks, got %d", len(files[0].hunks))
	}
	if files[0].path != "img.png" {
		t.Errorf("path = %q", files[0].path)
	}
}
