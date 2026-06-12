# hunky — usage notes & best practices

Field notes from using the tool to split mixed-concern commits across several repos.

## What it's for

Staging *some* hunks from a file's diff without the interactive `git add -p` TUI —
the one operation an agent or script can't drive. Use it **only when a file genuinely
mixes concerns** (e.g. a 2-line bugfix tangled into a feature change). For a file whose
changes are all one concern, plain `git add <file>` is simpler — don't reach for this.

## Order of operations (the reliable loop)

```
1. hunky --list <file>              # see numbered hunks + their @@ headers
2. hunky --dry-run <file> N M ...   # PREVIEW the exact patch before staging
3. hunky <file> N M ...             # stage those hunks (git apply --cached)
4. git diff --cached --stat                   # confirm what landed in the index
5. <commit>                                   # then the leftover hunks are the next commit
```

Step 2 (dry-run) is not optional for mixed files. Hunk headers (`@@ -1017,7 ...`) tell
you *where* but not *what*; a one-line crash fix can hide between feature hunks. Dry-run
shows the actual `+`/`-` lines so you stage the concern you mean to.

## The one real gotcha: hunk numbers are stable until you COMMIT

The tool reads `git diff HEAD` (HEAD vs **working tree**, which already includes whatever
you've staged). Consequences:

- **Staging more hunks does NOT renumber.** You can `--list` once and stage `1 3 5` then
  `2 4` against the same numbering — they stay put. Stage everything for a commit in one
  numbering pass.
- **Committing DOES renumber.** Once you commit, those changes leave the HEAD-vs-worktree
  diff, so the *next* `--list` starts over at 1 with only the remaining hunks. This is
  why you'll see "invalid hunk number 5 (have 4 hunks)" if you try to reuse old numbers
  after a commit — re-run `--list` after every commit.

Rule of thumb: **one `--list` per commit.** List → stage all this commit's hunks → commit
→ list again for the next.

## Selecting by line range instead of number

`--lines START-END` stages every hunk overlapping that range — handy when you know the
code region but not the hunk index:

```
hunky --lines 1017-1024 instance_detail_live.ex
```

## Selecting by complement: `--invert`

When most of a file is one concern and only a hunk or two belong elsewhere, name the few
and invert rather than listing all the rest. `--count` tells you how many there are:

```
hunky --count   instance_detail_live.ex     # e.g. 5
hunky --invert  instance_detail_live.ex 4   # stage hunks 1,2,3,5 (everything but 4)
```

## Don't hand-roll patches

We hit a `\1`-backref mangling bug trying to edit a patch with `sed` during this work.
The tool reconstructs the patch from parsed hunks and pipes it to `git apply --cached`
cleanly — let it. Never string-replace on patch bodies.

## Known limits (today)

- **Single file per invocation.** Multi-file diffs parse correctly and the `<file>`
  arg selects which one to operate on, but each run stages from one file.
- **Base is always `git diff HEAD`.** No `--base <ref>` mode, and by design: the patch is
  applied to the index via `git apply --cached`, so any base other than HEAD would fail to
  apply where the index and that base already differ. This is also why the renumber-after-
  commit behavior above is the model you work within.

## Worked example (from this session)

`instance_detail_live.ex` mixed a spec-upload-diff feature (hunks 1,2,3,5) with an
unrelated projection-click crash fix (hunk 4). To commit them separately:

```
hunky --list  instance_detail_live.ex          # 5 hunks
hunky --dry-run instance_detail_live.ex 4      # confirm hunk 4 == the crash fix
hunky instance_detail_live.ex 4                # stage just the fix
git diff --cached --stat                                  # 1 file, 2 insertions(+), 1 deletion
# commit "fix projection details click crash"
hunky --list instance_detail_live.ex           # NOW renumbered: 4 hunks left
# stage the rest + spec_tab.ex, commit the feature
```
