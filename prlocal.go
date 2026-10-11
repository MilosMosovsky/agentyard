package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	for _, pr := range prs {
		pr.Local = nil
		if path := paths[pr.Repo]; path != "" {
			local, key := c.check(path, pr, deadline)
			pr.Local = local
			if local.Checked {
				live[key] = local
			}
		}
	}
	c.pairs = live
}

func (c *prLocalCache) check(path string, pr *TrackedPR, deadline time.Time) (*PRLocal, prLocalKey) {
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
	env := []string{"GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=", "GIT_NO_REPLACE_OBJECTS=1"}
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
	local.HeadSha, err = resolve("refs/remotes/origin/" + pr.Branch)
	if err != nil || local.HeadSha != pr.HeadSha || pr.HeadSha == "" {
		return fail("the local copy of the branch is not at the PR's head yet")
	}
	local.BaseSha, err = resolve("refs/remotes/origin/" + pr.BaseRef)
	if err != nil || pr.BaseRef == "" {
		return fail("the local copy of base branch " + pr.BaseRef + " is missing or unavailable")
	}
	key.base, key.head = local.BaseSha, local.HeadSha
	if cached := c.pairs[key]; cached != nil {
		return cached, key
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
	out, err = g("merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", local.BaseSha, local.HeadSha)
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
	}
	local.Checked = true
	return local, key
}
