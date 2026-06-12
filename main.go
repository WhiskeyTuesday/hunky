// hunky — selectively stage hunks from a file's diff without a TUI.
//
// Usage:
//
//	hunky --list <file>             list hunks with numbers and line ranges
//	hunky <file> <hunk>...          stage specific hunks by number (1-based)
//	hunky --lines START-END <file>  stage whole hunks overlapping a line range
//	hunky --pick-lines A-B <file>   stage only the +/- lines within A-B (splits hunks)
//	hunky --dry-run <file> <hunk>.. print patch without staging
//
// Reads from 'git diff HEAD -- <file>' by default. Pass --stdin to read the diff
// from stdin instead; if that diff spans multiple files, the <file> arg selects
// which one to operate on.
//
// Examples:
//
//	hunky --list src/runtime.zig
//	hunky src/runtime.zig 4 7 8
//	hunky --lines 455-480 src/runtime.zig
//	git diff HEAD -- src/runtime.zig | hunky --stdin src/runtime.zig 4 7 8

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const version = "0.1.0"

type Hunk struct {
	header   string // full @@ line
	lines    []string
	oldStart int
	oldCount int
	newStart int
	newCount int
}

type FileDiff struct {
	path        string   // new-file path (from `+++ b/<path>`), used to match the <file> arg
	fileHeaders []string // diff --git, index, ---, +++
	hunks       []Hunk
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printUsage()
		os.Exit(0)
	}
	for _, a := range args {
		if a == "--version" || a == "-V" {
			fmt.Println("hunky " + version + " — man, I'm pretty.")
			os.Exit(0)
		}
	}

	var (
		list    bool
		count   bool
		dryRun  bool
		stdin   bool
		invert  bool
		lineRng string
		pickRng string
	)

	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--list":
			list = true
		case "--count":
			count = true
		case "--dry-run":
			dryRun = true
		case "--stdin":
			stdin = true
		case "--invert":
			invert = true
		case "--lines":
			i++
			if i >= len(args) {
				die("--lines requires an argument (e.g. --lines 455-480)")
			}
			lineRng = args[i]
		case "--pick-lines":
			i++
			if i >= len(args) {
				die("--pick-lines requires an argument (e.g. --pick-lines 25-37)")
			}
			pickRng = args[i]
		default:
			positional = append(positional, args[i])
		}
	}
	if lineRng != "" && pickRng != "" {
		die("--lines and --pick-lines are mutually exclusive")
	}
	if invert && pickRng != "" {
		die("--invert can't combine with --pick-lines (inversion is hunk-level)")
	}

	if len(positional) == 0 {
		die("missing <file> argument")
	}
	file := positional[0]
	hunkArgs := positional[1:]

	// Get the diff.
	var diffText []byte
	var err error
	if stdin {
		diffText, err = io.ReadAll(os.Stdin)
	} else {
		diffText, err = gitDiff(file)
	}
	if err != nil {
		die("failed to get diff: %v", err)
	}

	files, err := parseDiff(bytes.NewReader(diffText))
	if err != nil {
		die("failed to parse diff: %v", err)
	}
	fd, err := selectFile(files, file)
	if err != nil {
		die("%v", err)
	}
	if fd == nil {
		fmt.Fprintf(os.Stderr, "no diff for %s\n", file)
		os.Exit(0)
	}

	if count {
		fmt.Println(len(fd.hunks))
		return
	}

	if list {
		for i, h := range fd.hunks {
			fmt.Printf("%2d  %s\n", i+1, h.header)
		}
		return
	}

	// Line-level (sub-hunk) staging: reconstruct each hunk keeping only the
	// +/- changes whose worktree line number falls in the range. Splits a
	// hunk that mixes wanted and unwanted changes — which whole-hunk --lines
	// can't do when the changes aren't separated by a context line.
	if pickRng != "" {
		start, end, ok := parseRange(pickRng)
		if !ok {
			die("invalid --pick-lines value %q: expected START-END (e.g. 25-37)", pickRng)
		}
		patch, n := buildPartialPatch(fd, start, end)
		if n == 0 {
			die("no added/removed lines fall in lines %d-%d", start, end)
		}
		if dryRun {
			fmt.Print(patch)
			return
		}
		if err := applyPatch(patch); err != nil {
			die("git apply --cached failed: %v", err)
		}
		fmt.Fprintf(os.Stderr, "staged %d line-change(s) from %s (lines %d-%d)\n", n, file, start, end)
		return
	}

	// Determine which hunks to stage.
	var selected []int
	if lineRng != "" {
		start, end, ok := parseRange(lineRng)
		if !ok {
			die("invalid --lines value %q: expected START-END (e.g. 455-480)", lineRng)
		}
		for i, h := range fd.hunks {
			if h.newStart <= end && h.newStart+max(h.newCount-1, 0) >= start {
				selected = append(selected, i)
			}
		}
		if len(selected) == 0 {
			die("no hunks overlap lines %d-%d", start, end)
		}
	} else {
		if len(hunkArgs) == 0 {
			die("specify hunk numbers to stage, or use --list to see them, or --lines START-END")
		}
		for _, a := range hunkArgs {
			n, err := strconv.Atoi(a)
			if err != nil || n < 1 || n > len(fd.hunks) {
				die("invalid hunk number %q (have %d hunks; use --list to see them)", a, len(fd.hunks))
			}
			selected = append(selected, n-1)
		}
	}

	if invert {
		selected = invertSelection(selected, len(fd.hunks))
		if len(selected) == 0 {
			die("--invert leaves no hunks to stage (you named all of them)")
		}
	}

	patch := buildPatch(fd, selected)

	if dryRun {
		fmt.Print(patch)
		return
	}

	if err := applyPatch(patch); err != nil {
		die("git apply --cached failed: %v", err)
	}

	fmt.Fprintf(os.Stderr, "staged %d hunk(s) from %s\n", len(selected), file)
}

// invertSelection returns the hunk indices in [0,n) that are NOT in selected,
// in ascending order — the complement used by --invert.
func invertSelection(selected []int, n int) []int {
	in := make(map[int]bool, len(selected))
	for _, i := range selected {
		in[i] = true
	}
	var out []int
	for i := 0; i < n; i++ {
		if !in[i] {
			out = append(out, i)
		}
	}
	return out
}

func gitDiff(file string) ([]byte, error) {
	out, err := exec.Command("git", "diff", "HEAD", "--", file).Output()
	if err != nil {
		// exit 1 with no output means no diff — not an error
		if len(out) == 0 {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

func applyPatch(patch string) error {
	cmd := exec.Command("git", "apply", "--cached")
	cmd.Stdin = strings.NewReader(patch)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// parseDiff splits a unified diff into one FileDiff per `diff --git` section,
// each carrying its own file headers and hunks. Multi-file diffs (e.g. piped via
// --stdin) parse correctly; the caller picks the file to operate on.
func parseDiff(r io.Reader) ([]FileDiff, error) {
	scanner := bufio.NewScanner(r)
	// Diff lines can be long; raise the scanner's limit well above the 64KB default.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var files []FileDiff
	var cur *FileDiff
	var curHunk *Hunk

	flushHunk := func() {
		if cur != nil && curHunk != nil {
			cur.hunks = append(cur.hunks, *curHunk)
			curHunk = nil
		}
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, *cur)
			cur = nil
		}
	}

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "diff --git") {
			flushFile()
			cur = &FileDiff{fileHeaders: []string{line}, path: pathFromDiffGit(line)}
			continue
		}
		if cur == nil {
			continue // preamble before the first file (shouldn't happen for `git diff`)
		}

		if strings.HasPrefix(line, "@@ ") {
			flushHunk()
			h := Hunk{header: line}
			h.oldStart, h.oldCount, h.newStart, h.newCount = parseHunkHeader(line)
			curHunk = &h
			continue
		}

		if curHunk != nil {
			curHunk.lines = append(curHunk.lines, line)
			continue
		}

		// Between `diff --git` and the first `@@`: a file header line (index,
		// mode, ---, +++, new/deleted file). The `+++ b/<path>` line is the most
		// reliable source of the new-file path.
		cur.fileHeaders = append(cur.fileHeaders, line)
		if p := pathFromPlusLine(line); p != "" {
			cur.path = p
		}
	}
	flushFile()

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return files, nil
}

// pathFromDiffGit extracts the new-file path from `diff --git a/<old> b/<new>`.
// Returns "" if it can't (e.g. quoted paths with spaces — an uncommon edge case).
func pathFromDiffGit(line string) string {
	if i := strings.Index(line, " b/"); i >= 0 {
		return line[i+len(" b/"):]
	}
	return ""
}

// pathFromPlusLine extracts the path from `+++ b/<path>`. Returns "" for
// `+++ /dev/null` (a deletion) or non-`+++` lines.
func pathFromPlusLine(line string) string {
	if !strings.HasPrefix(line, "+++ ") {
		return ""
	}
	p := strings.TrimPrefix(line, "+++ ")
	if p == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(p, "b/")
}

// selectFile picks the FileDiff to operate on. A single-file diff (the common
// `git diff -- <file>` case) is used as-is. For a multi-file diff, the <file>
// arg selects by path (exact or path-suffix match); an ambiguous or missing
// match is an error.
func selectFile(files []FileDiff, arg string) (*FileDiff, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) == 1 {
		return &files[0], nil
	}
	var matches []int
	for i := range files {
		if pathMatches(files[i].path, arg) {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 1:
		return &files[matches[0]], nil
	case 0:
		return nil, fmt.Errorf("diff spans %d files and none match %q; pass a path matching one of them", len(files), arg)
	default:
		return nil, fmt.Errorf("%q matches %d files in the diff; pass a more specific path", arg, len(matches))
	}
}

func pathMatches(diffPath, arg string) bool {
	arg = strings.TrimPrefix(arg, "./")
	return diffPath == arg ||
		strings.HasSuffix(diffPath, "/"+arg) ||
		strings.HasSuffix(arg, "/"+diffPath)
}

func buildPatch(fd *FileDiff, selected []int) string {
	var b strings.Builder
	for _, h := range fd.fileHeaders {
		b.WriteString(h)
		b.WriteByte('\n')
	}
	for _, idx := range selected {
		h := fd.hunks[idx]
		b.WriteString(h.header)
		b.WriteByte('\n')
		for _, l := range h.lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// buildPartialPatch reconstructs a patch keeping only the +/- changes whose
// worktree (new-file) line number falls in [start,end]. Unselected additions
// are dropped; unselected deletions are demoted to context (so they are NOT
// staged but the surrounding patch still applies). Returns the patch and the
// number of +/- changes kept across all hunks.
func buildPartialPatch(fd *FileDiff, start, end int) (string, int) {
	var b strings.Builder
	for _, h := range fd.fileHeaders {
		b.WriteString(h)
		b.WriteByte('\n')
	}
	total := 0
	for _, h := range fd.hunks {
		rebuilt, kept := filterHunk(h, start, end)
		if kept == 0 {
			continue
		}
		total += kept
		b.WriteString(rebuilt)
	}
	return b.String(), total
}

// filterHunk rebuilds one hunk to contain only the +/- changes in [start,end]
// (worktree line numbers). Returns the rebuilt hunk text (with a recomputed
// header) and the count of +/- changes kept. A kept count of 0 means the hunk
// contributes nothing and should be omitted.
func filterHunk(h Hunk, start, end int) (string, int) {
	newLine := h.newStart
	var kept []string
	oldCount, newCount, changes := 0, 0, 0
	prevKept := true
	for _, l := range h.lines {
		if l == "" {
			kept = append(kept, l)
			oldCount++
			newCount++
			newLine++
			prevKept = true
			continue
		}
		switch l[0] {
		case '\\': // "\ No newline at end of file" — only keep if its line was kept
			if prevKept {
				kept = append(kept, l)
			}
		case ' ':
			kept = append(kept, l)
			oldCount++
			newCount++
			newLine++
			prevKept = true
		case '+':
			if newLine >= start && newLine <= end {
				kept = append(kept, l)
				newCount++
				changes++
				prevKept = true
			} else {
				prevKept = false
			}
			newLine++ // a '+' line always advances the worktree line counter
		case '-':
			if newLine >= start && newLine <= end {
				kept = append(kept, l)
				oldCount++
				changes++
			} else {
				// Not staging this deletion: the line survives, so it becomes
				// context (present on both sides).
				kept = append(kept, " "+l[1:])
				oldCount++
				newCount++
			}
			prevKept = true
			// a '-' line has no worktree line number; do not advance newLine
		default:
			kept = append(kept, l)
			prevKept = true
		}
	}
	if changes == 0 {
		return "", 0
	}
	var b strings.Builder
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.oldStart, oldCount, h.newStart, newCount)
	for _, l := range kept {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String(), changes
}

// parseHunkHeader parses @@ -oldStart[,oldCount] +newStart[,newCount] @@ ...
func parseHunkHeader(header string) (oldStart, oldCount, newStart, newCount int) {
	// e.g. "@@ -56,7 +56,7 @@ pub const Runtime = struct {"
	parts := strings.Fields(header)
	if len(parts) < 3 {
		return
	}
	oldStart, oldCount = parseRange2(strings.TrimPrefix(parts[1], "-"))
	newStart, newCount = parseRange2(strings.TrimPrefix(parts[2], "+"))
	return
}

func parseRange2(s string) (start, count int) {
	parts := strings.SplitN(s, ",", 2)
	start, _ = strconv.Atoi(parts[0])
	if len(parts) == 2 {
		count, _ = strconv.Atoi(parts[1])
	} else {
		count = 1
	}
	return
}

func parseRange(s string) (start, end int, ok bool) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return
	}
	var err error
	start, err = strconv.Atoi(parts[0])
	if err != nil {
		return
	}
	end, err = strconv.Atoi(parts[1])
	if err != nil {
		return
	}
	ok = start <= end
	return
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

func printUsage() {
	fmt.Print(`hunky — stage just the hunks you want, no TUI. (Man, those are some pretty hunks.)

Usage:
  hunky --list <file>               list hunks with numbers and @@ headers
  hunky --count <file>              print the number of hunks
  hunky <file> <hunk>...            stage hunks by number (1-based)
  hunky --invert <file> <hunk>...   stage every hunk EXCEPT the listed ones
  hunky --lines START-END <file>    stage whole hunks overlapping a line range
  hunky --pick-lines A-B <file>     stage only the +/- lines within A-B (splits a hunk)
  hunky --dry-run <file> <hunk>...  print the patch instead of staging
  hunky --stdin <file> <hunk>...    read the diff from stdin
  hunky --version                   print version

Examples:
  hunky --list src/runtime.zig
  hunky src/runtime.zig 4 7 8
  hunky --invert src/runtime.zig 4         # stage everything but hunk 4
  hunky --lines 455-480 src/runtime.zig
  hunky --pick-lines 25-37 CHANGELOG.md    # one changelog entry from an adjacent pair
  git diff HEAD -- src/runtime.zig | hunky --stdin src/runtime.zig 4 7 8
`)
}
