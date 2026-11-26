package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/stack"
)

// The pull request of each branch hydra shows, brief: its number, open,
// draft, merged or closed, the review and the checks. With 20 or 30
// worktrees, some run a branch whose PR was merged long ago: nothing is left
// to do there, and the developer wants to see which.
//
// hydra only reads. It asks GitHub's CLI (gh) once for every branch it shows
// (`gh pr list --search "head:a head:b ..."`, which GitHub ORs), every
// prAge, and never in the way of a snapshot: a watch asks in the background,
// and the answer is kept on disk so the next dashboard starts with it. gh is
// optional: without it, logged out or offline, there are no PR badges and
// one line says why. A merge GitHub did with a merge commit also shows
// without gh: the branch's commit is in origin/main, off its first-parent
// line (mergedByAncestry). A squash merge only gh can tell.

const (
	prAge        = 3 * time.Minute
	prWatchWait  = 20 * time.Second // api.github.com can be slow; the watch doesn't wait for it
	prStatusWait = 8 * time.Second  // `hydra status` and the MCP tools do
	prNewGap     = 15 * time.Second // the least time between two asks when a new branch shows up
	prChunk      = 25               // branches per gh call: a search query has a length limit
)

// snapPR is a branch's pull request, as `hydra status --json` gives it.
type snapPR struct {
	Number int    `json:"number,omitempty"` // 0: none known to GitHub, only mergedLocally
	State  string `json:"state"`            // open, draft, merged, closed
	Review string `json:"review,omitempty"` // approved, changes requested, review required
	Checks string `json:"checks,omitempty"` // passing, failing, pending
	URL    string `json:"url,omitempty"`
	Base   string `json:"base,omitempty"` // the branch it merges into
	// MergedLocally: the branch's commit is in origin's main, merged there
	// with a merge commit (git alone tells, no gh needed).
	MergedLocally bool `json:"mergedLocally,omitempty"`
}

// Done says nothing is left to do on the branch: its PR is merged or closed.
func (p *snapPR) Done() bool {
	return p != nil && (p.State == "merged" || p.State == "closed")
}

// Short is the PR in a few words: "#10265 draft", "#10265 ✗ checks".
func (p *snapPR) Short() string {
	if p == nil {
		return ""
	}
	num := ""
	if p.Number > 0 {
		num = fmt.Sprintf("#%d ", p.Number)
	}
	switch {
	case p.State != "open":
		return num + p.State
	case p.Checks == "failing":
		return num + "✗ checks"
	case p.Review == "changes requested":
		return num + "✗ review"
	case p.Checks == "pending":
		return num + "◐ checks"
	case p.Review == "approved":
		return num + "approved"
	case p.Checks == "passing":
		return num + "✓"
	}
	return num + "open"
}

// Long is the PR on one line: "#10265 open · review required · checks
// passing".
func (p *snapPR) Long() string {
	if p == nil {
		return ""
	}
	parts := []string{p.State}
	if p.Number > 0 {
		parts[0] = fmt.Sprintf("#%d %s", p.Number, p.State)
	} else if p.MergedLocally {
		parts[0] = "merged (its commits are in origin's main)"
	}
	if !p.Done() {
		if p.Review != "" {
			parts = append(parts, p.Review)
		}
		if p.Checks != "" {
			parts = append(parts, "checks "+p.Checks)
		}
	}
	return strings.Join(parts, " · ")
}

// snapPRSource says where the PR status comes from: when gh was last asked,
// and why there is none when gh can't be.
type snapPRSource struct {
	At    *time.Time `json:"at,omitempty"` // never asked yet: absent
	Error string     `json:"error,omitempty"`
}

// ghPR is one PR as `gh pr list --json` prints it.
type ghPR struct {
	Number            int       `json:"number"`
	State             string    `json:"state"` // OPEN, MERGED, CLOSED
	IsDraft           bool      `json:"isDraft"`
	HeadRefName       string    `json:"headRefName"`
	BaseRefName       string    `json:"baseRefName"`
	ReviewDecision    string    `json:"reviewDecision"`
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
	URL               string    `json:"url"`
	IsCrossRepository bool      `json:"isCrossRepository"`
}

// ghCheck is a check run (status, conclusion) or a commit status (state).
type ghCheck struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

const ghFields = "number,state,isDraft,headRefName,baseRefName,reviewDecision,statusCheckRollup,url,isCrossRepository"

// parsePRs reads gh's output: the PR of each wanted branch. A branch with
// several (one closed, a new one opened) gets the open one, else the newest.
// A fork's PR whose branch has the same name isn't this repository's branch.
func parsePRs(data []byte, want map[string]bool) (map[string]snapPR, error) {
	var list []ghPR
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("gh printed something that isn't a list of PRs: %w", err)
	}
	best := map[string]ghPR{}
	for _, p := range list {
		if !want[p.HeadRefName] || p.IsCrossRepository {
			continue
		}
		if have, ok := best[p.HeadRefName]; !ok || betterPR(p, have) {
			best[p.HeadRefName] = p
		}
	}
	out := map[string]snapPR{}
	for b, p := range best {
		out[b] = prOf(p)
	}
	return out, nil
}

func betterPR(a, b ghPR) bool {
	rank := func(p ghPR) int {
		switch p.State {
		case "OPEN":
			return 2
		case "MERGED":
			return 1
		}
		return 0
	}
	if rank(a) != rank(b) {
		return rank(a) > rank(b)
	}
	return a.Number > b.Number
}

func prOf(p ghPR) snapPR {
	out := snapPR{Number: p.Number, URL: p.URL, Base: p.BaseRefName, Checks: checksOf(p.StatusCheckRollup)}
	switch {
	case p.State == "MERGED":
		out.State = "merged"
	case p.State == "CLOSED":
		out.State = "closed"
	case p.IsDraft:
		out.State = "draft"
	default:
		out.State = "open"
	}
	out.Review = map[string]string{
		"APPROVED":          "approved",
		"CHANGES_REQUESTED": "changes requested",
		"REVIEW_REQUIRED":   "review required",
	}[p.ReviewDecision]
	return out
}

// checksOf rolls the checks up the way GitHub's badge does: failing when one
// failed, pending while one runs, passing when all passed (or were skipped).
// A cancelled run counts as neither: a newer one replaced it.
func checksOf(checks []ghCheck) string {
	if len(checks) == 0 {
		return ""
	}
	pending := false
	for _, c := range checks {
		switch {
		case c.State == "FAILURE" || c.State == "ERROR":
			return "failing"
		case c.Conclusion == "FAILURE" || c.Conclusion == "TIMED_OUT" || c.Conclusion == "ACTION_REQUIRED" || c.Conclusion == "STARTUP_FAILURE":
			return "failing"
		case c.State == "PENDING" || c.State == "EXPECTED":
			pending = true
		case c.State == "" && c.Status != "COMPLETED":
			pending = true
		}
	}
	if pending {
		return "pending"
	}
	return "passing"
}

// ghProgram is the gh to run: HYDRA_GH (a fake one, in tests and demos), or
// gh on PATH. HYDRA_GH=off turns PR status off.
func ghProgram() (string, error) {
	gh := os.Getenv("HYDRA_GH")
	switch gh {
	case "off":
		return "", errors.New("PR status is off (HYDRA_GH=off)")
	case "":
		gh = "gh"
	}
	path, err := exec.LookPath(gh)
	if err != nil {
		return "", errors.New("gh isn't installed: PR status needs GitHub's CLI (gh), and is optional")
	}
	return path, nil
}

// fetchPRs asks gh for the PRs of branches, in the repository at dir.
func fetchPRs(ctx context.Context, dir string, branches []string) (map[string]snapPR, error) {
	gh, err := ghProgram()
	if err != nil {
		return nil, err
	}
	if !hostedRemote(dir) {
		return nil, errNoHost
	}
	want := map[string]bool{}
	for _, b := range branches {
		want[b] = true
	}
	out := map[string]snapPR{}
	for i := 0; i < len(branches); i += prChunk {
		chunk := branches[i:min(i+prChunk, len(branches))]
		var q []string
		for _, b := range chunk {
			q = append(q, "head:"+b)
		}
		// Each branch can have had a few PRs (closed, then opened again).
		cmd := exec.CommandContext(ctx, gh, "pr", "list", "--state", "all", "--search", strings.Join(q, " "),
			"--json", ghFields, "--limit", fmt.Sprint(max(30, 3*len(chunk))))
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		data, err := cmd.Output()
		if err != nil {
			return nil, ghFailure(ctx, err, stderr.String())
		}
		prs, err := parsePRs(data, want)
		if err != nil {
			return nil, err
		}
		for b, p := range prs {
			out[b] = p
		}
	}
	return out, nil
}

// errNoHost: no remote is on a server, so there is nothing for gh to ask.
// Not a problem with gh: `hydra status` doesn't mention it.
var errNoHost = errors.New("the repository has no remote on a server (GitHub), so no PRs")

// hostedRemote says whether a remote is on a server, as opposed to a path
// on this machine (a demo's, a test's): only then can gh have PRs for it.
func hostedRemote(dir string) bool {
	names, err := gitOut(dir, "remote")
	if err != nil {
		return false
	}
	for _, name := range strings.Fields(names) {
		url, err := gitOut(dir, "remote", "get-url", name)
		if err != nil {
			continue
		}
		if strings.Contains(url, "@") || strings.Contains(url, "://") && !strings.HasPrefix(url, "file://") {
			return true
		}
	}
	return false
}

// ghFailure says why gh gave no PRs, in a line for the dashboard's help.
func ghFailure(ctx context.Context, err error, stderr string) error {
	first, _, _ := strings.Cut(strings.TrimSpace(stderr), "\n")
	switch {
	case ctx.Err() != nil:
		return errors.New("gh didn't answer in time: GitHub is slow or unreachable")
	case strings.Contains(stderr, "gh auth login") || strings.Contains(stderr, "not logged"):
		return errors.New("gh isn't logged in: gh auth login")
	case first != "":
		return errors.New("gh failed: " + first)
	}
	return fmt.Errorf("gh failed: %w", err)
}

// mergeTarget is what a merged branch's commits are in: origin's copy of the
// base branch, which a merge on GitHub moves first; the local one without
// an origin.
func mergeTarget(dir, base string) string {
	if _, err := gitOut(dir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+base); err == nil {
		return "origin/" + base
	}
	return base
}

// mergedByAncestry says whether the commit tip was merged into target with a
// merge commit: target has it, and not on its first-parent line. A branch
// just cut from main, with no commit of its own yet, is on that line, so it
// doesn't count; a squash or a rebase merge leaves no trace (gh tells those).
func mergedByAncestry(dir, tip, target string) bool {
	if tip == "" || !gitx.IsAncestor(dir, tip, target) {
		return false
	}
	// The first-parent commits of target that descend from tip, newest
	// first: the oldest is where tip came in. On the line, its first parent
	// is tip itself.
	out, err := gitOut(dir, "rev-list", "--first-parent", "--ancestry-path", tip+".."+target)
	if err != nil || out == "" {
		return false // tip is target: main itself
	}
	lines := strings.Split(out, "\n")
	parent, err := gitOut(dir, "rev-parse", lines[len(lines)-1]+"^1")
	return err == nil && parent != "" && parent != tip
}

// prCache is the last answer, on disk, so the first frame of the next
// dashboard (and a `hydra status` within prAge) has it without asking.
type prCache struct {
	At       time.Time         `json:"at"` // when gh was asked
	Error    string            `json:"error,omitempty"`
	Branches []string          `json:"branches"` // what it was asked about
	PRs      map[string]snapPR `json:"prs"`
}

// hydraCacheDir is where hydra keeps what it can work out again:
// HYDRA_CACHE_DIR, or the user's cache directory.
func hydraCacheDir() string {
	if d := os.Getenv("HYDRA_CACHE_DIR"); d != "" {
		return filepath.Join(d, "hydra")
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "hydra")
}

func prCachePath(mainRoot string) string {
	sum := sha256.Sum256([]byte(mainRoot))
	return filepath.Join(hydraCacheDir(), "prs-"+hex.EncodeToString(sum[:6])+".json")
}

func readPRCache(mainRoot string) *prCache {
	data, err := os.ReadFile(prCachePath(mainRoot))
	if err != nil {
		return nil
	}
	var c prCache
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	return &c
}

// writePRCache writes atomically: a dashboard and a `hydra status` can
// write at once.
func writePRCache(mainRoot string, c *prCache) {
	path := prCachePath(mainRoot)
	data, err := json.Marshal(c)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".prs-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(data)
	if tmp.Close() != nil || werr != nil {
		return
	}
	os.Rename(tmp.Name(), path)
}

// covers says whether the cache was asked about every one of branches.
func (c *prCache) covers(branches []string) bool {
	asked := map[string]bool{}
	for _, b := range c.Branches {
		asked[b] = true
	}
	for _, b := range branches {
		if !asked[b] {
			return false
		}
	}
	return true
}

// prSource keeps the PR status of one repository: gh's last answer, and
// which branches git alone says are merged.
type prSource struct {
	mainRoot string

	mu        sync.Mutex
	cache     *prCache
	merged    map[string]bool // by ancestry
	localDone bool
	busy      bool
}

func newPRSource(mainRoot string) *prSource {
	return &prSource{mainRoot: mainRoot, cache: readPRCache(mainRoot), merged: map[string]bool{}}
}

// due says whether gh is to be asked again: its answer is prAge old, or a
// branch it wasn't asked about showed up (a new worktree).
func (p *prSource) due(branches []string, force bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy {
		return false
	}
	c := p.cache
	switch {
	case force || c == nil:
		return true
	case time.Since(c.At) > prAge:
		return true
	}
	return !c.covers(branches) && time.Since(c.At) > prNewGap
}

// refresh asks gh about branches and works out which are merged by
// ancestry (heads: each branch's commit). wait does it before it returns;
// otherwise it runs in the background and done is called after.
func (p *prSource) refresh(branches []string, heads map[string]string, base string, wait bool, timeout time.Duration, done func()) {
	p.mu.Lock()
	if p.busy {
		p.mu.Unlock()
		return
	}
	p.busy = true
	p.mu.Unlock()
	branches = append([]string(nil), branches...)
	sort.Strings(branches)
	work := func() {
		p.local(branches, heads, base)
		if done != nil {
			done()
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		prs, err := fetchPRs(ctx, p.mainRoot, branches)
		cancel()
		p.mu.Lock()
		c := &prCache{At: time.Now(), Branches: branches, PRs: prs}
		if err != nil {
			c.Error = err.Error()
			if p.cache != nil {
				c.PRs = p.cache.PRs // the last ones known stay, with the reason they aren't newer
			}
		}
		p.cache = c
		p.busy = false
		p.mu.Unlock()
		writePRCache(p.mainRoot, c)
		if done != nil {
			done()
		}
	}
	if wait {
		work()
		return
	}
	go work()
}

// local works out which of branches git alone says are merged. Cheap: an
// is-ancestor each, and two more git commands for one that is.
func (p *prSource) local(branches []string, heads map[string]string, base string) {
	merged := map[string]bool{}
	if base != "" {
		target := mergeTarget(p.mainRoot, base)
		for _, b := range branches {
			merged[b] = mergedByAncestry(p.mainRoot, heads[b], target)
		}
	}
	p.mu.Lock()
	p.merged, p.localDone = merged, true
	p.mu.Unlock()
}

// claimLocal is true the first time it is called before local ran: for
// the one caller that is to run it.
func (p *prSource) claimLocal() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	first := !p.localDone
	p.localDone = true
	return first
}

// of is the PR of branch, nil when none is known.
func (p *prSource) of(branch string) *snapPR {
	if p == nil || branch == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var out *snapPR
	if p.cache != nil {
		if pr, ok := p.cache.PRs[branch]; ok {
			out = &pr
		}
	}
	if p.merged[branch] {
		if out == nil {
			out = &snapPR{State: "merged"}
		}
		out.MergedLocally = true
	}
	return out
}

// source says when gh was last asked and why it can't be, for the snapshot.
func (p *prSource) source() *snapPRSource {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		return &snapPRSource{}
	}
	at := p.cache.At
	return &snapPRSource{At: &at, Error: p.cache.Error}
}

// loadPRs is the PR status for a command that prints once (`hydra status`,
// the MCP tools): the cache when it is recent and covers the branches,
// otherwise gh, waited for (prStatusWait at most).
func loadPRs(mainRoot, base string, branches []string, heads map[string]string) *prSource {
	p := newPRSource(mainRoot)
	if p.due(branches, false) {
		p.refresh(branches, heads, base, true, prStatusWait, nil)
		return p
	}
	// gh's answer is recent: only the merges git tells, which cost nothing.
	p.local(branches, heads, base)
	return p
}

// shownPRs is the PR status of every branch hydra shows (the worktrees' and
// the stacks' branches with no checkout), for a command that prints once.
// The stacks are read from view, which may be nil.
func shownPRs(ms *mainState, wts []stack.Worktree, view *stackView) *prSource {
	main, base := ms.target.checkout.Root, ms.target.stack.Hydra.BaseRef
	var branches []string
	heads := map[string]string{}
	for _, wt := range wts {
		if wt.Root != main && wt.Branch != "" {
			branches = append(branches, wt.Branch)
			heads[wt.Branch] = wt.Head
		}
	}
	if view != nil {
		for _, b := range view.st.Bare {
			branches = append(branches, b.Name)
			heads[b.Name] = b.Head
		}
	}
	if len(branches) == 0 {
		return newPRSource(main)
	}
	return loadPRs(main, base, branches, heads)
}

// prText is a branch's PR for `hydra status`: what it is, the strong
// marker when nothing is left to do, and its link.
func prText(pr *snapPR, src *snapPRSource) string {
	switch {
	case pr == nil && src != nil && src.Error != "":
		return "unknown: " + src.Error
	case pr == nil:
		return "none for this branch"
	}
	text := pr.Long()
	if pr.Done() {
		text = "PR " + text + ": nothing left to do here"
	}
	if pr.URL != "" {
		text += "  " + pr.URL
	}
	return text
}

// prCell is a branch's PR in a table of `hydra status --all`.
func prCell(pr *snapPR) string {
	if pr == nil {
		return "-"
	}
	return pr.Short()
}

// any says whether one of branches has a PR known: `hydra status --all`
// has a PR column only then.
func (p *prSource) any(branches []string) bool {
	for _, b := range branches {
		if p.of(b) != nil {
			return true
		}
	}
	return false
}

// ghTrouble is why gh couldn't be asked, when that is worth a line: not
// for a repository with no remote on a server.
func (p *prSource) ghTrouble() string {
	if src := p.source(); src != nil && src.Error != "" && src.Error != errNoHost.Error() {
		return src.Error
	}
	return ""
}

// donePRs is the line of `hydra status --all` that names the branches whose
// PR is merged or closed; "" when there is none.
func donePRs(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("On a merged or closed PR, nothing left to do: %s.", strings.Join(names, ", "))
}
