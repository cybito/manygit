package aigit

import (
	"strings"
	"testing"
)

func ctx() Context {
	return Context{
		Root:   "/tree",
		Cursor: "blendxapi",
		Repos: []Repo{
			{Name: "blendxapi", Group: "edx-dev", Branch: "feat/x", MainRef: "master", Upstream: "origin/feat/x", Ahead: 2, Dirty: 3, Remote: true, Tag: "v1.1.1"},
			{Name: "blendxai", Group: "edx-dev", Branch: "main", MainRef: "main", Upstream: "origin/main", Remote: true},
			{Name: "blendxddn", Group: "other", Branch: "main", MainRef: "main"},
		},
		CursorBranch:   "feat/x",
		CursorBranches: []string{"feat/x", "master", "feat/filter"},
	}
}

// Everything the plan would be WRONG without has to survive into the text.
func TestRender_CarriesTheDecidingFacts(t *testing.T) {
	got := ctx().Render()
	for _, want := range []string{
		"blendxapi", "edx-dev", "feat/x",
		"master",        // per-repo main ref: rebasing blendxapi onto "main" would fail
		"origin/feat/x", // actual tracking ref for commands needing a remote name
		"3 dirty",       // cannot rebase a dirty tree
		"has-remote",    // the context must identify capability, not invent a remote name
		"v1.1.1",        // "cut the next tag" needs to know where it is
		"cursor repo: blendxapi",
		"groups: edx-dev, other",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("context is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "| remote |") {
		t.Fatalf("context must not present capability marker as a remote name:\n%s", got)
	}
}

func TestNames_CoversWhatYouCanType(t *testing.T) {
	names := ctx().Names()
	has := func(s string) bool {
		for _, n := range names {
			if n == s {
				return true
			}
		}
		return false
	}
	for _, want := range []string{"blendxapi", "other", "feat/filter", "main"} {
		if !has(want) {
			t.Errorf("Names() is missing %q — it would not autocomplete", want)
		}
	}
}

func TestComplete(t *testing.T) {
	names := ctx().Names()
	for _, tc := range []struct{ in, want string }{
		{"rebase blendxap", "i"},         // completes the last word only
		{"sync everything in ot", "her"}, // group folders complete too
		{"", ""},                         // nothing typed, nothing offered
		{"rebase ", ""},                  // trailing space: no word to complete
		{"zzz", ""},                      // no match
		{"BLENDXAP", "i"},                // case-insensitive match
		// "blendxa" prefixes BOTH blendxai and blendxapi, and that is already all
		// they share — so tab adds nothing and waits for the deciding character
		// rather than silently committing you to one of them.
		{"rebase blendxa", ""},
	} {
		if got := Complete(names, tc.in); got != tc.want {
			t.Errorf("Complete(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Tab fills in as far as every candidate agrees, then stops — one more character
// narrows the field and the next tab finishes the job. This is what a shell does.
func TestComplete_StopsAtTheCommonPrefix(t *testing.T) {
	names := []string{"featurebranch", "feat", "feature"}
	if got := Complete(names, "fea"); got != "t" {
		t.Errorf("Complete(fea) = %q, want %q — all three share only \"feat\"", got, "t")
	}
	// "featu" rules out "feat", leaving feature and featurebranch: they agree
	// through "feature".
	if got := Complete(names, "featu"); got != "re" {
		t.Errorf("Complete(featu) = %q, want %q", got, "re")
	}
	// "featureb" leaves exactly one, so it completes the whole name.
	if got := Complete(names, "featureb"); got != "ranch" {
		t.Errorf("Complete(featureb) = %q, want %q", got, "ranch")
	}
}

// A single match still completes fully — the common prefix of one name is itself.
func TestComplete_SingleMatchCompletesWholly(t *testing.T) {
	if got := Complete([]string{"blendxapi", "other"}, "blendx"); got != "api" {
		t.Errorf("Complete = %q, want %q", got, "api")
	}
}

func TestPrompt_StatesTheContract(t *testing.T) {
	p := Prompt(ctx(), "rebase current onto master", nil)
	for _, want := range []string{
		"rebase current onto master", // the request itself
		"blendxapi",                  // the context
		"Only git",                   // the decline instruction
		"never delete remote refs",   // the safety rules, restated
		"upstream",                   // actual tracking ref is available
		"literal remote name",        // prevents capability marker from becoming a Git argument
		"STOP at the first failure",  // ordering matters to the model
		"NEVER ask a question",       // there is no conversation here
		"no second turn",             // ...and the model is told why
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}
