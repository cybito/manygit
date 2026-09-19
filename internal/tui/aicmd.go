package tui

import (
	"context"
	"path/filepath"
	"strconv"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rabeeh-ta/manygit/internal/aigit"
	"github.com/rabeeh-ta/manygit/internal/git"
	"github.com/rabeeh-ta/manygit/internal/harness"
)

// aiTimeout bounds the harness call. Longer than the news summary's 90s would
// leave the prompt looking hung; shorter risks cutting off a slow first token.
const aiTimeout = 90 * time.Second

// askHarness is the seam tests replace. Nothing else in manygit stubs a harness,
// so this sits at the call site rather than inside the harness package, which
// keeps harness.OneShot and its own tests untouched.
var askHarness = func(ctx context.Context, h harness.Harness, dir, prompt string) (string, error) {
	return h.OneShot(ctx, dir, prompt)
}

// aiContext snapshots the tree for the harness. It copies plain values out of the
// repo view-models on the UI goroutine: a command closure must never hold a
// *repoVM, because background fetches mutate those and `go test -race` will say so.
func (m Model) aiContext() aigit.Context {
	c := aigit.Context{Root: m.root}
	for _, r := range m.repos {
		c.Repos = append(c.Repos, aigit.Repo{
			Name:     r.repo.Name,
			Group:    r.repo.Group,
			Branch:   r.status.Branch,
			MainRef:  r.status.Default,
			Upstream: r.status.Upstream,
			Ahead:    r.status.Ahead,
			Behind:   r.status.Behind,
			Dirty:    r.status.DirtyCount,
			Remote:   r.status.Slug != "" || r.status.Ahead > 0 || r.status.Behind > 0,
			Tag:      r.latestTag,
		})
	}
	for _, sc := range m.scripts {
		c.Scripts = append(c.Scripts, sc.Name)
	}
	if cur := m.currentVisible(m.visibleRepos()); cur != nil {
		c.Cursor = cur.repo.Name
		c.CursorBranch = cur.status.Branch
		for _, b := range m.branches {
			c.CursorBranches = append(c.CursorBranches, b.LocalName())
		}
	}
	return c
}

// aiPlanMsg carries the harness's reply back to the UI.
type aiPlanMsg struct {
	run    int
	prompt string
	plan   aigit.Plan
	err    error
}

// aiAskCmd sends the request to the harness off the UI goroutine. Everything it
// needs is captured by value before the closure is built.
func aiAskCmd(h harness.Harness, dir string, c aigit.Context, request string, run int) tea.Cmd {
	root := c.Root
	return func() tea.Msg {
		// Reading @-referenced files is disk IO, so it belongs here rather than in
		// the key handler. Attach refuses anything outside the tree; a refusal
		// still reaches the harness as a line saying the file was not read, so a
		// typo produces an explanation instead of a silent omission.
		atts := aigit.Attach(root, aigit.Refs(request))
		prompt := aigit.Prompt(c, request, atts)
		ctx, cancel := context.WithTimeout(context.Background(), aiTimeout)
		defer cancel()
		out, err := askHarness(ctx, h, dir, prompt)
		if err != nil {
			return aiPlanMsg{run: run, prompt: request, err: err}
		}
		plan, err := aigit.Parse(out)
		return aiPlanMsg{run: run, prompt: request, plan: plan, err: err}
	}
}

// aiDoneMsg carries the finished run's results.
type aiDoneMsg struct {
	run     int
	results []aigit.Result
}

// aiExecCmd runs a confirmed plan. The whole plan runs in one command rather than
// one command per step: the steps are sequential and stop at the first failure,
// so there is nothing to interleave, and a single message keeps the Output pane
// from showing a half-applied plan as if it were finished.
func aiExecCmd(root string, dirs map[string]string, plan aigit.Plan, run int) tea.Cmd {
	return func() tea.Msg {
		res := aigit.Execute(plan, func(repo string, args ...string) (string, error) {
			dir, ok := dirs[repo]
			if !ok {
				// The harness named a repo that isn't in the tree. Fail this step
				// rather than guessing at a path — guessing would run a command
				// somewhere the user never saw named.
				return "", &unknownRepoError{repo: repo}
			}
			return git.Run(dir, args...)
		})
		return aiDoneMsg{run: run, results: res}
	}
}

type unknownRepoError struct{ repo string }

func (e *unknownRepoError) Error() string { return "no repo named " + e.repo + " in this tree" }

// repoDirs maps repo name to absolute path for the executor.
func (m Model) repoDirs() map[string]string {
	dirs := make(map[string]string, len(m.repos))
	for _, r := range m.repos {
		dirs[r.repo.Name] = r.repo.Path
	}
	return dirs
}

// harnessDirOrRoot is where the harness process runs. It has no bearing on where
// git commands run — those use each repo's own path.
func (m Model) harnessDirOrRoot() string {
	if m.root != "" {
		return m.root
	}
	return filepath.Dir(".")
}

// plural renders "1 command" / "3 commands".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
