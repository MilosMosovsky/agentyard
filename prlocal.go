package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type PRLocal struct {
	Path       string   `json:"path"`
	Checked    bool     `json:"checked"`
	Note       string   `json:"note,omitempty"`
	BaseSha    string   `json:"baseSha,omitempty"`
	HeadSha    string   `json:"headSha,omitempty"`
	Ahead      int      `json:"ahead"`
	Behind     int      `json:"behind"`
	Conflicts  []string `json:"conflicts,omitempty"` // the first 20 of ConflictsN
	ConflictsN int      `json:"conflictsN,omitempty"`
	Superseded bool     `json:"superseded"`
	Overlap    []string `json:"overlap,omitempty"`
	OverlapN   int      `json:"overlapN,omitempty"`
}

type prLocalKey struct{ path, base, head string }
type prLocalCache struct{ pairs map[prLocalKey]*PRLocal }

func gitExit(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}

// Only a poll calls validate, under the tracker's mutex. Failed checks are
// retried next poll; successful pairs live only as long as an open PR uses them.
func (c *prLocalCache) validate(prs []*TrackedPR, snap *Snapshot, budget time.Duration) {
	paths := map[string]string{}
	if snap != nil {
		for _, repo := range snap.Repos {
			if paths[repo.Slug] == "" {
				paths[repo.Slug] = repo.Path
			}
		}
	}
	live := map[prLocalKey]*PRLocal{}
	deadline := time.Now().Add(budget)
	fetched := map[string]bool{}
	for _, pr := range prs {
		pr.Local = nil
		if path := paths[pr.Repo]; path != "" {
			// The scan fetches on its own clock, so GitHub's newest commits may
			// not be here yet: fetch each repository at most once a pass for them.
			refresh := func() {
				if !fetched[path] && time.Now().Before(deadline) {
					fetched[path] = true
					if err := prFetch(path, pr.Repo, false); err != nil {
						log.Printf("prs: fetch %s for local checks: %v", filepath.Base(path), err)
					}
				}
			}
			local, key := c.check(path, pr, deadline, refresh)
			pr.Local = local
			if local.Checked {
				live[key] = local
			}
		}
	}
	c.pairs = live
}

// prFetch is fetch; tests point it at their local remotes.
var prFetch = fetch

func (c *prLocalCache) check(path string, pr *TrackedPR, deadline time.Time, refresh func()) (*PRLocal, prLocalKey) {
	local := &PRLocal{Path: path}
	key := prLocalKey{path: path}
	fail := func(note string) (*PRLocal, prLocalKey) {
		local.Checked, local.Superseded, local.Note = false, false, note
		return local, key
	}
	if pr.CrossRepo {
		return fail("fork PR: its branch is not in the local clone")
	}
	if !time.Now().Before(deadline) {
		return fail("the local validation time limit was reached")
	}
	// Legacy grafts and replace refs rewrite history, and system attributes
	// can pick merge drivers: none may shape the proof.
	env := []string{"GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_GRAFT_FILE=" + os.DevNull, "GIT_ATTR_NOSYSTEM=1"}
	g := func(args ...string) (string, error) {
		left := time.Until(deadline)
		if left <= 0 {
			return "", errors.New("the local validation time limit was reached")
		}
		return runEnv(min(gitTimeout, left), path, env, "git", args...)
	}
	resolve := func(ref string) (string, error) {
		out, err := g("rev-parse", "--verify", ref+"^{commit}")
		return strings.TrimSpace(out), err
	}
	// Older Git can fetch missing objects even during reads. Refuse partial
	// clones before reading commits, and forbid transports for every command.
	promisor, err := g("config", "--get-regexp", `^(extensions\.partialclone|remote\..*\.promisor)$`)
	if err != nil && !gitExit(err, 1) {
		return fail("could not check whether the local clone has all its objects")
	}
	if strings.TrimSpace(promisor) != "" {
		return fail("partial clone: missing objects could require a fetch")
	}
	// The proof is against GitHub's own head and base commits, so a local
	// branch that lags or was force-pushed cannot stand in for either.
	if pr.HeadSha == "" || pr.BaseSha == "" {
		return fail("GitHub did not report the PR's head and base commits")
	}
	have := func(sha string) bool { _, err := resolve(sha); return err == nil }
	if !have(pr.HeadSha) || !have(pr.BaseSha) {
		refresh()
	}
	switch {
	case !have(pr.HeadSha):
		return fail("the PR's newest commit is not in the local clone yet")
	case !have(pr.BaseSha):
		return fail("the newest commit of base branch " + pr.BaseRef + " is not in the local clone yet")
	}
	local.HeadSha, local.BaseSha = pr.HeadSha, pr.BaseSha
	// Repository settings are read on every check, cached or not: they can
	// change while the commits stay the same.
	shallow, err := g("rev-parse", "--is-shallow-repository")
	if err != nil {
		return fail("could not check whether the local clone has its whole history")
	}
	// A configured driver can discard changes or run arbitrary commands in the
	// repository. Such a merge cannot prove that the PR is safe to close.
	drivers, err := g("config", "--get-regexp", `^merge\..*\.driver$`)
	if err != nil && !gitExit(err, 1) {
		return fail("could not check the repository's merge settings")
	}
	if strings.TrimSpace(drivers) != "" {
		return fail("the repository uses a custom merge driver")
	}
	unprovable, err := mergeSettings(g, path)
	if err != nil {
		return fail("could not check the repository's merge settings")
	}
	key.base, key.head = local.BaseSha, local.HeadSha
	if cached := c.pairs[key]; cached != nil {
		return cached, key
	}
	out, err := g("rev-list", "--left-right", "--count", local.BaseSha+"..."+local.HeadSha)
	counts := strings.Fields(out)
	if err != nil || len(counts) != 2 {
		return fail("could not compare the branch with its base")
	}
	local.Behind, err = strconv.Atoi(counts[0])
	if err != nil {
		return fail("could not count the base's commits")
	}
	local.Ahead, err = strconv.Atoi(counts[1])
	if err != nil {
		return fail("could not count the PR's commits")
	}
	out, err = g("merge-base", local.BaseSha, local.HeadSha)
	if err != nil {
		return fail("could not find a shared base for the branches")
	}
	ancestor := strings.TrimSpace(out)
	if strings.TrimSpace(shallow) == "true" {
		if note := shallowGap(g, local.BaseSha, local.HeadSha); note != "" {
			return fail(note)
		}
	}
	changed := func(sha string) ([]string, error) {
		out, err := g("diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", ancestor, sha, "--")
		return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), err
	}
	headFiles, err := changed(local.HeadSha)
	if err != nil {
		return fail("could not read the PR's changed files")
	}
	baseFiles, err := changed(local.BaseSha)
	if err != nil {
		return fail("could not read the base's changed files")
	}
	baseSet := map[string]bool{}
	for _, file := range baseFiles {
		baseSet[file] = file != ""
	}
	for _, file := range headFiles {
		if baseSet[file] {
			local.Overlap = append(local.Overlap, file)
		}
	}
	sort.Strings(local.Overlap)
	local.OverlapN = len(local.Overlap)
	local.Overlap = local.Overlap[:min(20, len(local.Overlap))]
	common, err := g("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fail("could not locate the repository's objects")
	}
	objects, err := os.MkdirTemp("", "agentyard-pr-objects-")
	if err != nil {
		return fail("could not create a temporary directory for the local merge")
	}
	defer os.RemoveAll(objects)
	env = append(env, "GIT_OBJECT_DIRECTORY="+objects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES="+strconv.Quote(filepath.Join(strings.TrimSpace(common), "objects")))
	baseTree, err := g("rev-parse", local.BaseSha+"^{tree}")
	if err != nil {
		return fail("could not read the base's tree")
	}
	if unprovable == "" {
		unprovable = treeAttributes(g, local.BaseSha, local.HeadSha)
	}
	// Renormalizing would run the repository's clean and smudge filters.
	out, err = g("-c", "merge.renormalize=false", "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", local.BaseSha, local.HeadSha)
	if err != nil && !gitExit(err, 1) {
		return fail("the local merge could not be checked (Git 2.38 or newer is required)")
	}
	parts := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(parts) == 0 || len(parts[0]) != len(local.BaseSha) {
		return fail("the local merge returned an incomplete result")
	}
	if err != nil {
		if len(parts) < 2 {
			return fail("the local merge could not list its conflicts")
		}
		local.Conflicts = parts[1:]
		local.ConflictsN = len(local.Conflicts)
		sort.Strings(local.Conflicts)
		local.Conflicts = local.Conflicts[:min(20, len(local.Conflicts))]
	} else {
		if len(parts) != 1 {
			return fail(fmt.Sprintf("the local merge returned %d unexpected paths", len(parts)-1))
		}
		local.Superseded = parts[0] == strings.TrimSpace(baseTree)
		if local.Superseded && unprovable != "" {
			local.Superseded = false
			local.Note = "cannot prove it is already in the base: " + unprovable
		}
	}
	local.Checked = true
	return local, key
}

// mergeSettings says why a clean merge here would not prove a PR adds
// nothing: a merge default or attributes outside the commits (a union merge
// keeps both sides, so a PR's deletion can vanish without a conflict).
func mergeSettings(g func(...string) (string, error), path string) (string, error) {
	def, err := g("config", "--get", "merge.default")
	if err != nil && !gitExit(err, 1) {
		return "", err
	}
	if strings.TrimSpace(def) != "" {
		return "merge.default is set", nil
	}
	common, err := g("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	files := []string{filepath.Join(strings.TrimSpace(common), "info", "attributes")}
	if global, err := g("config", "--path", "--get", "core.attributesFile"); err == nil && strings.TrimSpace(global) != "" {
		files = append(files, strings.TrimSpace(global))
	} else if err != nil && !gitExit(err, 1) {
		return "", err
	} else if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		files = append(files, filepath.Join(xdg, "git", "attributes"))
	} else if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".config", "git", "attributes"))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", err
		}
		if attrPattern.Match(b) {
			return tilde(f) + " sets merge or filter attributes", nil
		}
	}
	return "", nil
}

// treeAttributes is mergeSettings for the .gitattributes files in the two
// commits; any that mention merge or filter rule the proof out.
func treeAttributes(g func(...string) (string, error), commits ...string) string {
	for _, c := range commits {
		out, err := g("ls-tree", "-r", "-z", "--name-only", c)
		if err != nil {
			return "its .gitattributes files could not be read"
		}
		for _, p := range strings.Split(out, "\x00") {
			if p != ".gitattributes" && !strings.HasSuffix(p, "/.gitattributes") {
				continue
			}
			b, err := g("cat-file", "blob", c+":"+p)
			if err != nil {
				return "its .gitattributes files could not be read"
			}
			if attrPattern.MatchString(b) {
				return p + " sets merge or filter attributes"
			}
		}
	}
	return ""
}

var attrPattern = regexp.MustCompile(`(?i)\b(merge|filter)\b`)

// shallowGap says whether a shallow clone's cut-off lies between the two
// commits and their merge bases. If it does, a nearer merge base may be hidden
// behind it, and a merge from an older one can drop a PR's changes; a cut-off
// further back hides nothing the merge reads.
func shallowGap(g func(...string) (string, error), base, head string) string {
	const gap = "shallow clone: its history between the PR and its base is incomplete"
	common, err := g("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return gap
	}
	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(common), "shallow"))
	if err != nil {
		return gap
	}
	cut := map[string]bool{}
	for _, sha := range strings.Fields(string(b)) {
		cut[sha] = true
	}
	bases, err := g("merge-base", "--all", base, head)
	if err != nil {
		return gap
	}
	args := []string{"rev-list", base, head}
	for _, sha := range strings.Fields(bases) {
		args = append(args, "^"+sha) // a cut-off at a merge base hides only older history
	}
	between, err := g(args...)
	if err != nil {
		return gap
	}
	for _, sha := range strings.Fields(between) {
		if cut[sha] {
			return gap
		}
	}
	return ""
}
