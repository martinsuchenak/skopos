package worker

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// Branch names (tmp/branch-handover — design.md §2). An item works on the
// branch Martin named (item.branch) or, without one, on the name the
// repository's template gives it. Either may already exist on origin: the
// item then takes it over (a handover, or a review-only item's subject).

// defaultBranchTemplate is the name every item got before templates:
// prefix, the item id's tail, a slug of the title.
const defaultBranchTemplate = "{prefix}{tail}-{slug}"

// validateBranchTemplate checks a repo's branch_template: known
// placeholders only, and {tail} or {ref} in it, so two items never get the
// same name and take over each other's branch.
func validateBranchTemplate(t string) error { return wsconfig.ValidateBranchTemplate(t) }

// validBranch is the worker's guard on every branch name it checks out or
// pushes: letters, digits, '.', '_', '/', '-'; no "..", "//", trailing '/'
// or '.', no component starting with '.' or ending in ".lock" (git's own
// rules, so a name never reaches git as an option or a revision range).
func validBranch(name string) bool {
	if len(name) > 200 || !reBranch.MatchString(name) ||
		strings.Contains(name, "..") || strings.Contains(name, "//") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// idTail is the last two segments of a UUIDv7 text id: unique enough and
// time-recognizable without the full 36 characters.
func idTail(itemID string) string {
	segs := strings.Split(itemID, "-")
	if len(segs) >= 2 {
		return segs[len(segs)-2] + "-" + segs[len(segs)-1]
	}
	return itemID
}

func (r *Repo) template() string {
	if r.BranchTemplate != "" {
		return r.BranchTemplate
	}
	return defaultBranchTemplate
}

// renderTemplate fills the template; upTo, when non-empty, stops at that
// placeholder (the stem other branches of the same reference share).
func renderTemplate(t string, item *Item, prefix, upTo string) string {
	if upTo != "" {
		if i := strings.Index(t, upTo); i >= 0 {
			t = t[:i]
		}
	}
	return strings.NewReplacer(
		"{prefix}", prefix,
		"{tail}", idTail(item.ID),
		"{slug}", branchSlug(item.Title),
		"{ref}", item.Ref,
	).Replace(t)
}

// branchFor is the branch the item works on, without asking git: the
// item's own branch, else the template's name. The name must carry the
// repo's branch prefix — the push guard's rule, checked here so a bad name
// stops the item before any work.
func branchFor(item *Item, repo *Repo) (string, error) {
	name := strings.TrimSpace(item.Branch)
	if name == "" {
		t := repo.template()
		if strings.Contains(t, "{ref}") && strings.TrimSpace(item.Ref) == "" {
			return "", fmt.Errorf("this repository names branches after the item's reference (%s): set the item's reference, or its branch", t)
		}
		name = renderTemplate(t, item, repo.BranchPrefix, "")
	}
	if !validBranch(name) {
		return "", fmt.Errorf("%q is not a usable branch name", name)
	}
	if repo.BranchPrefix != "" && !strings.HasPrefix(name, repo.BranchPrefix) {
		return "", fmt.Errorf("branch %q does not start with this repository's branch prefix %q: the worker only pushes branches that do", name, repo.BranchPrefix)
	}
	if isProtected(name, repo.Protected) {
		return "", fmt.Errorf("branch %q matches a protected pattern", name)
	}
	return name, nil
}

// branchStem is the part of the template's name before {slug}: other
// branches of the same reference start with it (WR203345-). Empty when the
// item names its branch, or the template has no {slug}.
func branchStem(item *Item, repo *Repo) string {
	t := repo.template()
	if strings.TrimSpace(item.Branch) != "" || !strings.Contains(t, "{slug}") {
		return ""
	}
	return renderTemplate(t, item, repo.BranchPrefix, "{slug}")
}

// branchChoice is what resolveBranch found.
type branchChoice struct {
	Name     string
	Exists   bool   // on origin: take it over
	Question string // several candidates and no exact match: Martin picks
}

// resolveBranch picks the item's branch and whether it exists on origin
// (design §2). Without a branch on the item, other branches sharing the
// template's stem make it a question: the worker never guesses which of
// someone's branches to continue.
func resolveBranch(ctx context.Context, repo *Repo, item *Item) (branchChoice, error) {
	name, err := branchFor(item, repo)
	if err != nil {
		return branchChoice{}, err
	}
	stem := branchStem(item, repo)
	pattern := "refs/heads/" + name
	if stem != "" {
		pattern = "refs/heads/" + stem + "*"
	}
	heads, err := remoteHeads(ctx, repo.Path, pattern)
	if err != nil {
		return branchChoice{}, err
	}
	for _, h := range heads {
		if h == name {
			return branchChoice{Name: name, Exists: true}, nil
		}
	}
	if len(heads) == 0 {
		return branchChoice{Name: name}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Branches starting with `%s` already exist: ", stem)
	for i, h := range heads {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("`" + h + "`")
	}
	fmt.Fprintf(&b, ". Set the item's branch to the one to continue, or to a new name (for example `%s`) to start a new branch, then retry.", name)
	return branchChoice{Name: name, Question: b.String()}, nil
}

// remoteHeads lists origin's branches matching a refs/heads pattern.
func remoteHeads(ctx context.Context, clone, pattern string) ([]string, error) {
	out, err := git(ctx, clone, "ls-remote", "--heads", "origin", pattern)
	if err != nil {
		return nil, fmt.Errorf("listing origin's branches: %w", err)
	}
	var heads []string
	for _, line := range strings.Split(out, "\n") {
		if _, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
			if h := strings.TrimPrefix(ref, "refs/heads/"); validBranch(h) {
				heads = append(heads, h)
			}
		}
	}
	sort.Strings(heads)
	return heads, nil
}

// fetchBranch fetches origin's branch into its remote-tracking ref (never
// FETCH_HEAD, which another worker process sharing the clone may
// overwrite) and returns its head.
func fetchBranch(ctx context.Context, clone, branch string) (string, error) {
	if !validBranch(branch) {
		return "", fmt.Errorf("refusing branch name %q", branch)
	}
	remote := "refs/remotes/origin/" + branch
	if _, err := git(ctx, clone, "fetch", "--quiet", "origin", "+refs/heads/"+branch+":"+remote); err != nil {
		return "", retryable(fmt.Errorf("fetching %s: %w", branch, err))
	}
	return git(ctx, clone, "rev-parse", "--verify", remote+"^{commit}")
}

// knowledgeBase is where an item's agents read the repository's
// instructions, skills and focus briefs: the fork point of base from the
// default branch. For an ordinary item that is the base itself; for a
// handover (base = the branch head) it is the default-branch commit the
// branch grew from, so a branch never briefs its own agents.
func knowledgeBase(ctx context.Context, repo *Repo, base string) string {
	if onDefaultBranch(ctx, repo, base) {
		return base
	}
	if mb, err := git(ctx, repo.Path, "merge-base", base, "origin/"+repo.DefaultBranch); err == nil && mb != "" {
		return mb
	}
	return base
}

// onDefaultBranch reports whether sha is in the default branch's history
// (an ordinary base) rather than only on a branch (a handover's base). A
// worker that didn't plan the item may have an origin/<default> older than
// the base, so a miss refreshes it once before answering.
func onDefaultBranch(ctx context.Context, repo *Repo, sha string) bool {
	def := "origin/" + repo.DefaultBranch
	if isAncestor(ctx, repo.Path, sha, def) {
		return true
	}
	if _, err := git(ctx, repo.Path, "fetch", "--quiet", "origin", "+refs/heads/"+repo.DefaultBranch+":refs/remotes/"+def); err != nil {
		return false
	}
	return isAncestor(ctx, repo.Path, sha, def)
}

// handover is what the planner is told about a branch it takes over.
type handover struct {
	Branch    string
	Head      string
	ForkPoint string
	Commits   string // oneline log, newest first
	Diffstat  string
	Diff      string
	Truncated bool
}

const (
	handoverMaxCommits   = 50
	handoverMaxDiff      = 60 << 10
	handoverMaxDiffLines = 20000
)

// diffLines is the number of changed lines between two commits (binary
// files count as one); on error, more than any limit.
func diffLines(ctx context.Context, clone, from, to string) int {
	out, err := git(ctx, clone, "diff", "--numstat", from, to)
	if err != nil {
		return 1 << 30
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		a, errA := strconv.Atoi(f[0])
		d, errD := strconv.Atoi(f[1])
		if errA != nil || errD != nil {
			n++ // binary
			continue
		}
		n += a + d
	}
	return n
}

// handoverContext reads the work already on a branch: its commits since the
// fork point and their diff, cut to a size a prompt can carry (the planner
// has no shell: past the cut it reads the changed files the diffstat lists).
func handoverContext(ctx context.Context, repo *Repo, branch, head string) (*handover, error) {
	fork := knowledgeBase(ctx, repo, head)
	h := &handover{Branch: branch, Head: head, ForkPoint: fork}
	var err error
	if h.Commits, err = git(ctx, repo.Path, "log", "--oneline", "--no-decorate", fmt.Sprintf("-%d", handoverMaxCommits), fork+".."+head); err != nil {
		return nil, err
	}
	if h.Diffstat, err = git(ctx, repo.Path, "diff", "--stat", fork, head); err != nil {
		return nil, err
	}
	// Size first: a branch that adds vendored or generated files would
	// otherwise be read whole into memory only to be cut.
	if diffLines(ctx, repo.Path, fork, head) > handoverMaxDiffLines {
		h.Truncated = true
		return h, nil
	}
	if h.Diff, err = git(ctx, repo.Path, "diff", fork, head); err != nil {
		return nil, err
	}
	if len(h.Diff) > handoverMaxDiff {
		cut := strings.LastIndex(h.Diff[:handoverMaxDiff], "\ndiff --git ")
		if cut <= 0 {
			cut = handoverMaxDiff
		}
		h.Diff, h.Truncated = h.Diff[:cut], true
	}
	return h, nil
}
