package aigit

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Repo is the snapshot of one repository the harness is told about. It is plain
// data on purpose: the caller copies it off the UI's own structs, so nothing here
// can be mutated by an in-flight fetch while a command goroutine reads it.
type Repo struct {
	Name     string // "blendxapi"
	Group    string // parent folder, e.g. "edx-dev" — how "everything in other/" resolves
	Branch   string // current branch
	MainRef  string // "main" or "master" — "rebase onto master" is wrong in a main repo
	Upstream string // actual tracking ref, e.g. "origin/main"; empty when none
	Ahead    int
	Behind   int
	Dirty    int  // changed files; you cannot rebase or pull cleanly
	Remote   bool // at least one remote is configured; not a remote name
	Tag      string
}

// Context is everything the harness gets about the tree.
type Context struct {
	Root           string
	Repos          []Repo
	Cursor         string   // repo under the cursor — the default scope
	CursorBranch   string   // branch highlighted in the Branches pane
	CursorBranches []string // branches of the cursor repo only; every repo's would be waste
	Scripts        []string // script paths relative to root, completed after "@"
}

// Groups returns the distinct group folders, sorted — the vocabulary for
// "sync everything in other/".
func (c Context) Groups() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range c.Repos {
		if r.Group != "" && !seen[r.Group] {
			seen[r.Group] = true
			out = append(out, r.Group)
		}
	}
	sort.Strings(out)
	return out
}

// Render writes the context as a compact table. One line per repo keeps a
// 40-repo tree small enough to send whole, and the header names the columns so
// the model does not have to guess what the numbers mean.
func (c Context) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "root: %s\n", c.Root)
	if gs := c.Groups(); len(gs) > 0 {
		fmt.Fprintf(&b, "groups: %s\n", strings.Join(gs, ", "))
	}
	fmt.Fprintf(&b, "cursor repo: %s\n", c.Cursor)
	if c.CursorBranch != "" {
		fmt.Fprintf(&b, "cursor branch: %s\n", c.CursorBranch)
	}
	if len(c.CursorBranches) > 0 {
		fmt.Fprintf(&b, "branches of %s: %s\n", c.Cursor, strings.Join(c.CursorBranches, ", "))
	}
	b.WriteString("\nrepos (name | group | branch | upstream | main-ref | ahead/behind | dirty | has-remote | latest-tag):\n")
	for _, r := range c.Repos {
		remote := "no"
		if r.Remote {
			remote = "yes"
		}
		tag := r.Tag
		if tag == "" {
			tag = "-"
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s | +%d/-%d | %d dirty | %s | %s\n",
			r.Name, orDash(r.Group), orDash(r.Branch), orDash(r.Upstream), orDash(r.MainRef),
			r.Ahead, r.Behind, r.Dirty, remote, tag)
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Names returns every completable word: repo names, group folders, and the
// branches known for the cursor repo plus each repo's current branch. The `:`
// input completes against this.
func (c Context) Names() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, r := range c.Repos {
		add(r.Name)
	}
	for _, g := range c.Groups() {
		add(g)
	}
	for _, b := range c.CursorBranches {
		add(b)
	}
	for _, r := range c.Repos {
		add(r.Branch)
	}
	// Scripts complete WITH their @, so typing "@upd" offers the reference form
	// rather than a bare path that would not be picked up as one.
	for _, s := range c.Scripts {
		add("@" + s)
	}
	return out
}

// Complete returns the remaining text that would finish the last word of s
// against Names, or "" if nothing matches. It is a display concern only — the
// raw typed text is what gets sent.
//
// It completes to the LONGEST COMMON PREFIX of every match, the way a shell
// does, rather than picking one of them. With blendxai and blendxapi in the
// tree, "blendxa" is already the common prefix, so tab adds nothing and waits
// for you to type the character that decides it; "blendxap" then completes the
// whole name. Picking the shortest full match instead would quietly commit you
// to blendxai when you meant the other one.
func Complete(names []string, s string) string {
	word := lastWord(s)
	if word == "" {
		return ""
	}
	lower := strings.ToLower(word)
	lcp, found := "", false
	for _, n := range names {
		if !strings.HasPrefix(strings.ToLower(n), lower) {
			continue
		}
		if !found {
			lcp, found = n, true
			continue
		}
		lcp = commonPrefix(lcp, n)
	}
	if !found || len(lcp) <= len(word) {
		return "" // nothing matched, or the prefix is all they have in common
	}
	return lcp[len(word):]
}

// commonPrefix is the shared leading text of a and b, cut on a rune boundary so
// a multi-byte character is never split in half.
func commonPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	// i == len(a) means a is wholly a prefix of b, which is already a boundary.
	for i > 0 && i < len(a) && !utf8.RuneStart(a[i]) {
		i--
	}
	return a[:i]
}

func lastWord(s string) string {
	if i := strings.LastIndexAny(s, " \t"); i >= 0 {
		return s[i+1:]
	}
	return s
}
