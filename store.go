// In-memory state: repositories, issues, pulls, labels, comments, and the
// git objects behind them. All behaviour mirrors the reference forge.
package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Label struct {
	ID          int
	Name        string
	Color       string
	Description string
}

type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // open | closed
	Labels     []*Label
	CreatedSeq int64
	CreatedAt  string
	UpdatedAt  string
	JSONCache  []byte
}

type Comment struct {
	ID        int
	Body      string
	CreatedAt string
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // open | closed
	HeadRef        string
	BaseRef        string
	HeadSha        string
	BaseSha        string
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
	CreatedSeq     int64
	CreatedAt      string
	UpdatedAt      string
}

type PackCacheEntry struct {
	Pack      []byte
	Shallow   []string
	Unshallow []string
	Encoded   []byte
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Private       bool
	Description   string
	DefaultBranch string
	Objects       map[string]*GitObject
	Refs          map[string]string
	Issues        map[int]*Issue
	IssueOrder    []int
	Pulls         map[int]*Pull
	PullOrder     []int
	Labels        map[string]*Label
	LabelOrder    []string
	Comments      map[int][]*Comment
	NextNumber    int
	NextCommentID int
	CreatedSeq    int64
	CreatedAt     string
	Version       int
	RepoJSONCache []byte
	PackCache     map[string]*PackCacheEntry
	PackCacheOrder []string
	CommitSeq     map[string]int
	NextCommitSeq int
	// advertisement caches (git smart HTTP)
	Adv2Version int
	Adv2Body    []byte
	Adv0        map[string]adv0Entry
	LsRefsCache map[string]lsRefsEntry
	CacheMu     sync.Mutex
}

type adv0Entry struct {
	Version int
	Body    []byte
}

type lsRefsEntry struct {
	Version int
	Body    []byte
}

type Store struct {
	Mu            sync.RWMutex
	Repos         map[string]*Repo
	NextRepoID    int
	NextObjectID  int
	NextCommentID int
}

func createStore() *Store {
	return &Store{
		Repos:         map[string]*Repo{},
		NextRepoID:    10000,
		NextObjectID:  100000,
		NextCommentID: 1,
	}
}

func repoKey(owner, name string) string { return owner + "/" + name }

func fullName(repo *Repo) string { return repo.Owner + "/" + repo.Name }

// repoJSON renders the REST form of a repo (key order is part of the wire
// contract, so this builds an ordered value).
func repoJSON(repo *Repo) *JVal {
	return JObjVal(
		JMember{"id", JIntVal(int64(repo.ID))},
		JMember{"name", JStrVal(repo.Name)},
		JMember{"full_name", JStrVal(fullName(repo))},
		JMember{"owner", JObjVal(
			JMember{"login", JStrVal(repo.Owner)},
			JMember{"id", JIntVal(1000)},
			JMember{"type", JStrVal("User")},
		)},
		JMember{"private", JBoolVal(repo.Private)},
		JMember{"description", JStrVal(repo.Description)},
		JMember{"default_branch", JStrVal(repo.DefaultBranch)},
		JMember{"created_at", JStrVal(repo.CreatedAt)},
		JMember{"updated_at", JStrVal(repo.CreatedAt)},
		JMember{"html_url", JStrVal("https://github.com/" + fullName(repo))},
		JMember{"clone_url", JStrVal("https://github.com/" + fullName(repo) + ".git")},
		JMember{"has_issues", JBoolVal(true)},
		JMember{"fork", JBoolVal(false)},
	)
}

func repoRestJSON(repo *Repo) []byte {
	if repo.RepoJSONCache == nil {
		repo.RepoJSONCache = repoJSON(repo).Marshal()
	}
	return repo.RepoJSONCache
}

func issueRestJSON(repo *Repo, issue *Issue) *JVal {
	labels := make([]*JVal, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, JObjVal(
			JMember{"id", JIntVal(int64(l.ID))},
			JMember{"name", JStrVal(l.Name)},
			JMember{"color", JStrVal(l.Color)},
			JMember{"description", JStrVal(l.Description)},
		))
	}
	return JObjVal(
		JMember{"id", JIntVal(int64(issue.ID))},
		JMember{"number", JIntVal(int64(issue.Number))},
		JMember{"title", JStrVal(issue.Title)},
		JMember{"body", JStrVal(issue.Body)},
		JMember{"state", JStrVal(issue.State)},
		JMember{"labels", JArrVal(labels...)},
		JMember{"created_at", JStrVal(issue.CreatedAt)},
		JMember{"updated_at", JStrVal(issue.UpdatedAt)},
		JMember{"repository_url", JStrVal("/api/v3/repos/" + repo.Owner + "/" + repo.Name)},
		JMember{"url", JStrVal(fmt.Sprintf("/api/v3/repos/%s/%s/issues/%d", repo.Owner, repo.Name, issue.Number))},
	)
}

func issueRestBuffer(repo *Repo, issue *Issue) []byte {
	if issue.JSONCache == nil {
		issue.JSONCache = issueRestJSON(repo, issue).Marshal()
	}
	return issue.JSONCache
}

func pullRestJSON(repo *Repo, pull *Pull) *JVal {
	mergedAt := JNullVal()
	if pull.MergedAt != nil {
		mergedAt = JStrVal(*pull.MergedAt)
	}
	mergeSha := JNullVal()
	if pull.MergeCommitSha != nil {
		mergeSha = JStrVal(*pull.MergeCommitSha)
	}
	return JObjVal(
		JMember{"id", JIntVal(int64(pull.ID))},
		JMember{"number", JIntVal(int64(pull.Number))},
		JMember{"title", JStrVal(pull.Title)},
		JMember{"body", JStrVal(pull.Body)},
		JMember{"state", JStrVal(pull.State)},
		JMember{"head", JObjVal(
			JMember{"ref", JStrVal(pull.HeadRef)},
			JMember{"sha", JStrVal(pull.HeadSha)},
			JMember{"label", JStrVal(repo.Owner + ":" + pull.HeadRef)},
		)},
		JMember{"base", JObjVal(
			JMember{"ref", JStrVal(pull.BaseRef)},
			JMember{"sha", JStrVal(pull.BaseSha)},
			JMember{"label", JStrVal(repo.Owner + ":" + pull.BaseRef)},
		)},
		JMember{"merged", JBoolVal(pull.Merged)},
		JMember{"merged_at", mergedAt},
		JMember{"merge_commit_sha", mergeSha},
		JMember{"created_at", JStrVal(pull.CreatedAt)},
		JMember{"updated_at", JStrVal(pull.UpdatedAt)},
		JMember{"html_url", JStrVal(fmt.Sprintf("https://github.com/%s/pull/%d", fullName(repo), pull.Number))},
	)
}

func createRepo(store *Store, owner, name string, private bool, description string) *Repo {
	key := repoKey(owner, name)
	if _, exists := store.Repos[key]; exists {
		return nil
	}
	seq := tick()
	repo := &Repo{
		ID:            store.NextRepoID,
		Owner:         owner,
		Name:          name,
		Private:       private,
		Description:   description,
		DefaultBranch: "main",
		Objects:       map[string]*GitObject{},
		Refs:          map[string]string{},
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*Pull{},
		Labels:        map[string]*Label{},
		Comments:      map[int][]*Comment{},
		NextNumber:    1,
		NextCommentID: 1,
		CreatedSeq:    seq,
		CreatedAt:     isoOf(seq),
		Version:       0,
		PackCache:     map[string]*PackCacheEntry{},
		CommitSeq:     map[string]int{},
		NextCommitSeq: 0,
		Adv0:          map[string]adv0Entry{},
		LsRefsCache:   map[string]lsRefsEntry{},
	}
	store.NextRepoID++
	store.Repos[key] = repo
	return repo
}

func getRepo(store *Store, owner, name string) *Repo {
	return store.Repos[repoKey(owner, name)]
}

func deleteRepo(store *Store, owner, name string) bool {
	key := repoKey(owner, name)
	if _, ok := store.Repos[key]; !ok {
		return false
	}
	delete(store.Repos, key)
	return true
}

func listReposByOwner(store *Store, owner string) []*Repo {
	var out []*Repo
	for _, repo := range store.Repos {
		if repo.Owner == owner {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedSeq < out[j].CreatedSeq })
	return out
}

// --- git objects ---

func putObject(repo *Repo, typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, exists := repo.Objects[oid]; !exists {
		repo.Objects[oid] = &GitObject{Type: typ, Data: data}
		if typ == "commit" {
			repo.CommitSeq[oid] = repo.NextCommitSeq
			repo.NextCommitSeq++
		}
	}
	return oid
}

func getObject(repo *Repo, oid string) *GitObject {
	return repo.Objects[oid]
}

func lookupOf(repo *Repo) ObjectLookup {
	return func(oid string) *GitObject { return repo.Objects[oid] }
}

// resolveRef resolves a ref name (short, full, or HEAD) to its oid.
func resolveRef(repo *Repo, name string) (string, bool) {
	if name == "HEAD" {
		oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		return oid, ok
	}
	if strings.HasPrefix(name, "refs/") {
		oid, ok := repo.Refs[name]
		return oid, ok
	}
	oid, ok := repo.Refs["refs/heads/"+name]
	return oid, ok
}

// peelToCommit follows annotated tags to the commit they point at.
func peelToCommit(repo *Repo, oid string) (string, bool) {
	return peelToCommitWith(lookupOf(repo), oid)
}

// treeWithChanges builds a new tree from a base tree with path changes
// applied (nil data deletes a path).
func treeWithChanges(repo *Repo, baseTreeOid *string, changes map[string][]byte, deletes map[string]bool) string {
	flat := map[string]TreeEntry{}
	if baseTreeOid != nil {
		flattenTree(*baseTreeOid, lookupOf(repo), flat)
	}
	for path, data := range changes {
		if deletes[path] {
			delete(flat, path)
		} else {
			flat[path] = TreeEntry{Oid: putObject(repo, "blob", data), Mode: "100644"}
		}
	}
	// Rebuild bottom-up: group paths into a directory tree.
	type dir struct {
		files map[string]string
		dirs  map[string]*dir
	}
	newDir := func() *dir { return &dir{files: map[string]string{}, dirs: map[string]*dir{}} }
	root := newDir()
	for path, entry := range flat {
		segments := strings.Split(path, "/")
		d := root
		for i := 0; i < len(segments)-1; i++ {
			child, ok := d.dirs[segments[i]]
			if !ok {
				child = newDir()
				d.dirs[segments[i]] = child
			}
			d = child
		}
		d.files[segments[len(segments)-1]] = entry.Oid
	}
	var build func(d *dir) string
	build = func(d *dir) string {
		var entries []TreeEntry
		for name, oid := range d.files {
			entries = append(entries, TreeEntry{Mode: "100644", Name: name, Oid: oid})
		}
		for name, child := range d.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, Oid: build(child)})
		}
		return putObject(repo, "tree", buildTree(entries))
	}
	return build(root)
}

func createCommit(repo *Repo, tree string, parents []string, message string) string {
	ts := tick()
	identity := viewerLogin + " <" + viewerEmail + ">"
	data := buildCommit(tree, parents, message, identity, identity, ts)
	return putObject(repo, "commit", data)
}

type commitListEntry struct {
	Sha     string
	Message string
	Seq     int
}

// listCommits returns commits reachable from a tip, newest first (by store
// sequence).
func listCommits(repo *Repo, tip string) []commitListEntry {
	seen := map[string]bool{}
	queue := []string{tip}
	var out []commitListEntry
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		commit := parseCommit(obj.Data)
		seq := repo.CommitSeq[oid]
		out = append(out, commitListEntry{Sha: oid, Message: commit.Message, Seq: seq})
		queue = append(queue, commit.Parents...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	return out
}

// --- issues ---

func createIssue(store *Store, repo *Repo, title, body string, labelNames []string) (*Issue, string) {
	var labels []*Label
	for _, name := range labelNames {
		label, ok := repo.Labels[name]
		if !ok {
			return nil, "unknown label " + name
		}
		labels = append(labels, label)
	}
	seq := tick()
	at := isoOf(seq)
	issue := &Issue{
		ID:         store.NextObjectID,
		Number:     repo.NextNumber,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     labels,
		CreatedSeq: seq,
		CreatedAt:  at,
		UpdatedAt:  at,
	}
	store.NextObjectID++
	repo.NextNumber++
	repo.Issues[issue.Number] = issue
	repo.IssueOrder = append(repo.IssueOrder, issue.Number)
	return issue, ""
}

func listIssues(repo *Repo, state, sortBy, direction string) []*Issue {
	var out []*Issue
	for _, number := range repo.IssueOrder {
		issue := repo.Issues[number]
		if state == "all" || issue.State == state {
			out = append(out, issue)
		}
	}
	if direction == "" {
		direction = "desc"
	}
	if sortBy == "created" || sortBy == "" {
		sort.SliceStable(out, func(i, j int) bool {
			if direction == "asc" {
				return out[i].CreatedSeq < out[j].CreatedSeq
			}
			return out[i].CreatedSeq > out[j].CreatedSeq
		})
	}
	return out
}

func patchIssue(repo *Repo, number int, state, title, body *string) *Issue {
	issue, ok := repo.Issues[number]
	if !ok {
		return nil
	}
	if state != nil && (*state == "open" || *state == "closed") {
		issue.State = *state
	}
	if title != nil {
		issue.Title = *title
	}
	if body != nil {
		issue.Body = *body
	}
	issue.UpdatedAt = isoOf(tick())
	issue.JSONCache = nil
	return issue
}

func createComment(repo *Repo, issueNumber int, body string) *Comment {
	if _, ok := repo.Issues[issueNumber]; !ok {
		return nil
	}
	comment := &Comment{ID: repo.NextCommentID, Body: body, CreatedAt: isoOf(tick())}
	repo.NextCommentID++
	repo.Comments[issueNumber] = append(repo.Comments[issueNumber], comment)
	return comment
}

func createLabel(repo *Repo, name, color string) *Label {
	if existing, ok := repo.Labels[name]; ok {
		return existing
	}
	if color == "" {
		color = "ededed"
	}
	label := &Label{ID: len(repo.Labels) + 1, Name: name, Color: color, Description: ""}
	repo.Labels[name] = label
	repo.LabelOrder = append(repo.LabelOrder, name)
	return label
}

// --- pull requests ---

func createPull(repo *Repo, head, base, title, body string) (*Pull, string) {
	headSha, okH := resolveRef(repo, head)
	baseSha, okB := resolveRef(repo, base)
	if !okH || !okB {
		return nil, "unknown head or base ref"
	}
	for _, number := range repo.PullOrder {
		existing := repo.Pulls[number]
		if existing.State == "open" && existing.HeadRef == head && existing.BaseRef == base {
			return nil, "a pull request already exists for this head"
		}
	}
	seq := tick()
	at := isoOf(seq)
	pull := &Pull{
		ID:         repo.ID*100000 + repo.NextNumber,
		Number:     repo.NextNumber,
		Title:      title,
		Body:       body,
		State:      "open",
		HeadRef:    head,
		BaseRef:    base,
		HeadSha:    headSha,
		BaseSha:    baseSha,
		CreatedSeq: seq,
		CreatedAt:  at,
		UpdatedAt:  at,
	}
	repo.NextNumber++
	repo.Pulls[pull.Number] = pull
	repo.PullOrder = append(repo.PullOrder, pull.Number)
	return pull, ""
}

func listPulls(repo *Repo, state string) []*Pull {
	var out []*Pull
	for _, number := range repo.PullOrder {
		pull := repo.Pulls[number]
		if state == "all" || pull.State == state {
			out = append(out, pull)
		}
	}
	return out
}

// lineDiff counts added/removed lines between two texts (prefix+suffix diff).
func lineDiff(oldText, newText string) (additions, deletions int) {
	if oldText == newText {
		return 0, 0
	}
	var oldLines, newLines []string
	if oldText != "" {
		oldLines = strings.Split(oldText, "\n")
	}
	if newText != "" {
		newLines = strings.Split(newText, "\n")
	}
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	return len(newLines) - prefix - suffix, len(oldLines) - prefix - suffix
}

type PullFileEntry struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

// pullFiles lists the changed files of a pull request: merge-base tree vs
// head tree.
func pullFiles(repo *Repo, pull *Pull) []PullFileEntry {
	lookup := lookupOf(repo)
	headCommit, okH := peelToCommit(repo, pull.HeadSha)
	baseCommit, okB := peelToCommit(repo, pull.BaseSha)
	if !okH || !okB {
		return nil
	}
	base, hasBase := mergeBase(baseCommit, headCommit, lookup)
	var baseTree *string
	if hasBase {
		if obj := repo.Objects[base]; obj != nil {
			t := parseCommit(obj.Data).Tree
			baseTree = &t
		}
	}
	headTree := parseCommit(repo.Objects[headCommit].Data).Tree
	var baseTreeVal string
	hasBaseTree := false
	if baseTree != nil {
		baseTreeVal, hasBaseTree = *baseTree, true
	}
	diffs := diffTrees(baseTreeVal, hasBaseTree, headTree, lookup)
	var out []PullFileEntry
	for _, d := range diffs {
		oldText := ""
		if d.HasOld {
			if obj := repo.Objects[d.OldOid]; obj != nil {
				oldText = string(obj.Data)
			}
		}
		newText := ""
		if d.HasNew {
			if obj := repo.Objects[d.NewOid]; obj != nil {
				newText = string(obj.Data)
			}
		}
		additions, deletions := lineDiff(oldText, newText)
		status := "modified"
		if !d.HasOld {
			status = "added"
		} else if !d.HasNew {
			status = "removed"
		}
		out = append(out, PullFileEntry{Filename: d.Path, Status: status, Additions: additions, Deletions: deletions, Changes: additions + deletions})
	}
	return out
}

// pullMergeable reports whether merging would conflict (three-way, per file,
// against the CURRENT base tip).
func pullMergeable(repo *Repo, pull *Pull) bool {
	lookup := lookupOf(repo)
	headCommit, okH := peelToCommit(repo, pull.HeadSha)
	baseTipOid, _ := resolveRef(repo, pull.BaseRef)
	if baseTipOid == "" {
		baseTipOid = pull.BaseSha
	}
	baseTip, okT := peelToCommit(repo, baseTipOid)
	if !okH || !okT {
		return false
	}
	base, hasBase := mergeBase(baseTip, headCommit, lookup)
	if !hasBase {
		return false
	}
	baseFlat := map[string]TreeEntry{}
	oursFlat := map[string]TreeEntry{}
	theirsFlat := map[string]TreeEntry{}
	flattenTree(parseCommit(repo.Objects[base].Data).Tree, lookup, baseFlat)
	flattenTree(parseCommit(repo.Objects[baseTip].Data).Tree, lookup, oursFlat)
	flattenTree(parseCommit(repo.Objects[headCommit].Data).Tree, lookup, theirsFlat)
	paths := map[string]bool{}
	for p := range baseFlat {
		paths[p] = true
	}
	for p := range oursFlat {
		paths[p] = true
	}
	for p := range theirsFlat {
		paths[p] = true
	}
	for path := range paths {
		b, hasB := baseFlat[path]
		o, hasO := oursFlat[path]
		t, hasT := theirsFlat[path]
		bOid := ""
		if hasB {
			bOid = b.Oid
		}
		oOid := ""
		if hasO {
			oOid = o.Oid
		}
		tOid := ""
		if hasT {
			tOid = t.Oid
		}
		_ = hasB
		_ = hasO
		_ = hasT
		if tOid == bOid {
			continue
		}
		if oOid == bOid {
			continue
		}
		if oOid == tOid {
			continue
		}
		return false
	}
	return true
}

func mergePull(repo *Repo, number int) (string, string, int) {
	pull, ok := repo.Pulls[number]
	if !ok {
		return "", "pull request not found", 404
	}
	if pull.State != "open" {
		return "", "pull request is not open", 405
	}
	if !pullMergeable(repo, pull) {
		return "", "merge conflict", 405
	}
	lookup := lookupOf(repo)
	headCommit, _ := peelToCommit(repo, pull.HeadSha)
	baseTipOid, _ := resolveRef(repo, pull.BaseRef)
	if baseTipOid == "" {
		baseTipOid = pull.BaseSha
	}
	baseTip, _ := peelToCommit(repo, baseTipOid)
	base, _ := mergeBase(baseTip, headCommit, lookup)
	baseFlat := map[string]TreeEntry{}
	oursFlat := map[string]TreeEntry{}
	theirsFlat := map[string]TreeEntry{}
	flattenTree(parseCommit(repo.Objects[base].Data).Tree, lookup, baseFlat)
	flattenTree(parseCommit(repo.Objects[baseTip].Data).Tree, lookup, oursFlat)
	flattenTree(parseCommit(repo.Objects[headCommit].Data).Tree, lookup, theirsFlat)
	merged := map[string]TreeEntry{}
	for p, e := range oursFlat {
		merged[p] = e
	}
	for path, entry := range theirsFlat {
		bOid := ""
		if b, ok := baseFlat[path]; ok {
			bOid = b.Oid
		}
		oOid := ""
		if o, ok := oursFlat[path]; ok {
			oOid = o.Oid
		}
		if entry.Oid != bOid && oOid == bOid {
			merged[path] = entry
		}
	}
	for path := range baseFlat {
		bOid := baseFlat[path].Oid
		oOid := ""
		if o, ok := oursFlat[path]; ok {
			oOid = o.Oid
		}
		if _, hasT := theirsFlat[path]; !hasT && oOid == bOid {
			delete(merged, path)
		}
	}
	changes := map[string][]byte{}
	deletes := map[string]bool{}
	current := map[string]TreeEntry{}
	flattenTree(parseCommit(repo.Objects[baseTip].Data).Tree, lookup, current)
	for path, entry := range merged {
		cur, ok := current[path]
		if !ok || cur.Oid != entry.Oid {
			if obj := repo.Objects[entry.Oid]; obj != nil {
				changes[path] = obj.Data
			} else {
				deletes[path] = true
				changes[path] = nil
			}
		}
	}
	for path := range current {
		if _, ok := merged[path]; !ok {
			deletes[path] = true
			changes[path] = nil
		}
	}
	baseTree := parseCommit(repo.Objects[baseTip].Data).Tree
	tree := treeWithChanges(repo, &baseTree, changes, deletes)
	sha := createCommit(repo, tree, []string{baseTip}, fmt.Sprintf("Merge pull request #%d from %s\n\n%s\n", pull.Number, pull.HeadRef, pull.Title))
	repo.Refs["refs/heads/"+pull.BaseRef] = sha
	repo.Version++
	pull.State = "closed"
	pull.Merged = true
	mergedAt := isoOf(tick())
	pull.MergedAt = &mergedAt
	pull.MergeCommitSha = &sha
	updatedAt := isoOf(tick())
	pull.UpdatedAt = updatedAt
	return sha, "", 0
}

// --- contents API ---

type ContentFile struct {
	Name    string
	Path    string
	Sha     string
	Size    int
	Content []byte
}

type ContentDirEntry struct {
	Type string // file | dir
	Name string
	Path string
	Sha  string
	Size int
	HasSize bool
}

func listTreeDir(repo *Repo, rootTree, dirPath string) []ContentDirEntry {
	var segments []string
	if dirPath != "" {
		segments = strings.Split(dirPath, "/")
	}
	treeOid := rootTree
	for _, segment := range segments {
		tree := repo.Objects[treeOid]
		if tree == nil {
			return nil
		}
		var found *TreeEntry
		for _, e := range parseTree(tree.Data) {
			if e.Name == segment && e.Mode == "40000" {
				ee := e
				found = &ee
				break
			}
		}
		if found == nil {
			return nil
		}
		treeOid = found.Oid
	}
	tree := repo.Objects[treeOid]
	if tree == nil {
		return nil
	}
	var out []ContentDirEntry
	for _, entry := range parseTree(tree.Data) {
		childPath := entry.Name
		if dirPath != "" {
			childPath = dirPath + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			out = append(out, ContentDirEntry{Type: "dir", Name: entry.Name, Path: childPath, Sha: entry.Oid})
		} else {
			size := 0
			if blob := repo.Objects[entry.Oid]; blob != nil {
				size = len(blob.Data)
			}
			out = append(out, ContentDirEntry{Type: "file", Name: entry.Name, Path: childPath, Sha: entry.Oid, Size: size, HasSize: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type readContentResult struct {
	File *ContentFile
	Dir  []ContentDirEntry
	IsDir bool
	Found bool
}

// readContent reads a path from a branch's tree: a file, a directory
// listing, or not found.
func readContent(repo *Repo, path string, ref *string) readContentResult {
	var tipOid string
	var ok bool
	if ref == nil {
		tipOid, ok = resolveRef(repo, repo.DefaultBranch)
	} else {
		tipOid, ok = resolveRef(repo, *ref)
	}
	if !ok {
		return readContentResult{}
	}
	commitOid, ok := peelToCommit(repo, tipOid)
	if !ok {
		return readContentResult{}
	}
	commit, ok := repo.Objects[commitOid]
	if !ok {
		return readContentResult{}
	}
	rootTree := parseCommit(commit.Data).Tree
	clean := strings.Trim(path, "/")
	if clean == "" {
		entries := listTreeDir(repo, rootTree, "")
		if entries == nil {
			return readContentResult{}
		}
		return readContentResult{Dir: entries, IsDir: true, Found: true}
	}
	flat := map[string]TreeEntry{}
	flattenTree(rootTree, lookupOf(repo), flat)
	if direct, ok := flat[clean]; ok {
		blob := repo.Objects[direct.Oid]
		if blob == nil {
			return readContentResult{}
		}
		name := clean
		if i := strings.LastIndex(clean, "/"); i >= 0 {
			name = clean[i+1:]
		}
		return readContentResult{File: &ContentFile{Name: name, Path: clean, Sha: direct.Oid, Size: len(blob.Data), Content: blob.Data}, Found: true}
	}
	prefix := clean + "/"
	isDir := false
	for key := range flat {
		if strings.HasPrefix(key, prefix) {
			isDir = true
			break
		}
	}
	if !isDir {
		return readContentResult{}
	}
	entries := listTreeDir(repo, rootTree, clean)
	if entries == nil {
		return readContentResult{}
	}
	return readContentResult{Dir: entries, IsDir: true, Found: true}
}

func putContent(repo *Repo, path, contentB64, message, sha, branch string, hasSha, hasBranch bool) (contentSha, commitSha, errMsg string, status int) {
	clean := strings.TrimLeft(path, "/")
	if !hasBranch {
		branch = repo.DefaultBranch
	}
	tipOid, ok := resolveRef(repo, branch)
	if !ok {
		if baseTip, ok2 := resolveRef(repo, repo.DefaultBranch); ok2 {
			repo.Refs["refs/heads/"+branch] = baseTip
			tipOid, ok = baseTip, true
		}
	}
	data := b64Decode(contentB64)
	if ok {
		commitOid, ok2 := peelToCommit(repo, tipOid)
		if !ok2 {
			return "", "", "branch has no commit", 422
		}
		commit := repo.Objects[commitOid]
		rootTree := parseCommit(commit.Data).Tree
		flat := map[string]TreeEntry{}
		flattenTree(rootTree, lookupOf(repo), flat)
		existing, hasExisting := flat[clean]
		if !hasSha {
			if hasExisting {
				return "", "", "file already exists", 422
			}
		} else {
			if !hasExisting {
				return "", "", "file does not exist", 422
			}
			if existing.Oid != sha {
				return "", "", "sha does not match", 409
			}
		}
		changes := map[string][]byte{clean: data}
		tree := treeWithChanges(repo, &rootTree, changes, map[string]bool{})
		commitSha := createCommit(repo, tree, []string{commitOid}, message)
		repo.Refs["refs/heads/"+branch] = commitSha
		repo.Version++
		return hashObject("blob", data), commitSha, "", 0
	}
	tree := treeWithChanges(repo, nil, map[string][]byte{clean: data}, map[string]bool{})
	commitSha = createCommit(repo, tree, []string{}, message)
	repo.Refs["refs/heads/"+branch] = commitSha
	repo.Version++
	return hashObject("blob", data), commitSha, "", 0
}

func deleteContent(repo *Repo, path, message, sha, branch string, hasBranch bool) (commitSha, errMsg string, status int) {
	clean := strings.TrimLeft(path, "/")
	if !hasBranch {
		branch = repo.DefaultBranch
	}
	tipOid, ok := resolveRef(repo, branch)
	if !ok {
		return "", "branch not found", 404
	}
	commitOid, ok := peelToCommit(repo, tipOid)
	if !ok {
		return "", "branch has no commit", 422
	}
	commit := repo.Objects[commitOid]
	rootTree := parseCommit(commit.Data).Tree
	flat := map[string]TreeEntry{}
	flattenTree(rootTree, lookupOf(repo), flat)
	existing, hasExisting := flat[clean]
	if !hasExisting {
		return "", "file does not exist", 404
	}
	if existing.Oid != sha {
		return "", "sha does not match", 409
	}
	changes := map[string][]byte{clean: nil}
	deletes := map[string]bool{clean: true}
	tree := treeWithChanges(repo, &rootTree, changes, deletes)
	commitSha = createCommit(repo, tree, []string{commitOid}, message)
	repo.Refs["refs/heads/"+branch] = commitSha
	repo.Version++
	return commitSha, "", 0
}
