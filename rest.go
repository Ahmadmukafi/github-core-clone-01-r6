// REST API v3: repositories, issues, labels, pulls, branches, commits,
// contents, and the git refs/tags endpoints. Every response is built to be
// byte-identical to the reference forge's.
package main

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
)

var restRequestSeq int64

func restJSON(status int, value *JVal, extra map[string]string) HttpResult {
	headers := map[string]string{
		"Content-Type":        "application/json; charset=utf-8",
		"X-GitHub-Request-Id": restRequestID(),
	}
	for k, v := range extra {
		headers[k] = v
	}
	return HttpResult{Status: status, Headers: headers, Body: value.Marshal()}
}

func restJSONBuffer(status int, body []byte, extra map[string]string) HttpResult {
	headers := map[string]string{
		"Content-Type":        "application/json; charset=utf-8",
		"X-GitHub-Request-Id": restRequestID(),
	}
	for k, v := range extra {
		headers[k] = v
	}
	return HttpResult{Status: status, Headers: headers, Body: body}
}

func restRequestID() string {
	seq := atomic.AddInt64(&restRequestSeq, 1)
	return "req_" + intStr(int(seq)) + "_" + intStr(int(tick()))
}

func restError(status int, message string) HttpResult {
	return restJSON(status, JObjVal(
		JMember{"message", JStrVal(message)},
		JMember{"documentation_url", JStrVal("https://docs.github.com/rest")},
	), nil)
}

var bearerRe = regexp.MustCompile(`^Bearer\s+(.+)$`)

// checkAuth implements the bearer check: any non-empty token locally, exact
// match when MAKEFUN_SHOP_TOKEN is set.
func checkAuth(headers map[string]string) (ok bool, status int) {
	expected := os.Getenv("MAKEFUN_SHOP_TOKEN")
	header := headers["authorization"]
	m := bearerRe.FindStringSubmatch(strings.TrimSpace(header))
	if expected != "" {
		if m == nil || m[1] != expected {
			return false, 401
		}
		return true, 0
	}
	if m == nil || m[1] == "" {
		return false, 401
	}
	return true, 0
}

func userJSON(login string) *JVal {
	id := int64(2000)
	if login == viewerLogin {
		id = 1000
	}
	return JObjVal(
		JMember{"login", JStrVal(login)},
		JMember{"id", JIntVal(id)},
		JMember{"type", JStrVal("User")},
		JMember{"avatar_url", JStrVal("https://github.com/" + login + ".png")},
		JMember{"html_url", JStrVal("https://github.com/" + login)},
		JMember{"url", JStrVal("/api/v3/users/" + login)},
	)
}

func ownerOr404(owner string) *HttpResult {
	if owner != viewerLogin {
		r := restError(404, "Not Found: user "+owner)
		return &r
	}
	return nil
}

type QueryParams map[string][]string

func (q QueryParams) Get(name string) string {
	if vals, ok := q[name]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

func (q QueryParams) Has(name string) bool {
	vals, ok := q[name]
	return ok && len(vals) > 0
}

type RestRequest struct {
	Method   string
	Segments []string
	Query    QueryParams
	Headers  map[string]string
	Body     []byte
}

// parseJSONBody parses a request body; empty is fine, anything that is not a
// JSON object is malformed.
func parseJSONBody(body []byte) (*JVal, bool) {
	if len(body) == 0 {
		return nil, false
	}
	parsed, err := ParseJSON(body)
	if err != nil || parsed.Kind != JObj {
		return nil, true
	}
	return parsed, false
}

func strVal(v *JVal) string {
	if v != nil && v.Kind == JStr {
		return v.Str
	}
	return ""
}

func strField(obj *JVal, name string) string {
	if obj == nil {
		return ""
	}
	return strVal(obj.ObjGet(name))
}

func handleRest(store *Store, req RestRequest) HttpResult {
	if ok, status := checkAuth(req.Headers); !ok {
		return restError(status, "Requires authentication")
	}
	segs := req.Segments
	var first, second, third string
	if len(segs) > 0 {
		first = segs[0]
	}
	if len(segs) > 1 {
		second = segs[1]
	}
	if len(segs) > 2 {
		third = segs[2]
	}

	// GET /api/v3/ — root
	if len(segs) == 0 {
		if req.Method != "GET" {
			return restError(404, "Not Found")
		}
		return restJSON(200, JObjVal(
			JMember{"current_user_url", JStrVal("/api/v3/user")},
			JMember{"repository_url", JStrVal("/api/v3/repos/{owner}/{repo}")},
		), map[string]string{"X-OAuth-Scopes": "repo, read:org"})
	}

	// GET /api/v3/meta
	if first == "meta" && len(segs) == 1 {
		if req.Method != "GET" {
			return restError(404, "Not Found")
		}
		return restJSON(200, JObjVal(
			JMember{"verifiable_password_authentication", JBoolVal(true)},
			JMember{"installed_version", JStrVal("1.0.0")},
			JMember{"github_services_sha", JStrVal(zeroOid)},
			JMember{"hooks", JArrVal(JStrVal("pre-receive"), JStrVal("post-receive"))},
			JMember{"git", JArrVal(JStrVal("127.0.0.1"))},
			JMember{"pages", JArrVal(JStrVal("127.0.0.1"))},
		), nil)
	}

	// GET /api/v3/user
	if first == "user" && len(segs) == 1 {
		if req.Method != "GET" {
			return restError(404, "Not Found")
		}
		return restJSON(200, userJSON(viewerLogin), nil)
	}

	// /api/v3/user/repos
	if first == "user" && second == "repos" && len(segs) == 2 {
		if req.Method == "GET" {
			perPage := clampPage(jsParseIntOr(req.Query.Get("per_page"), 30), 30)
			if perPage > 100 {
				perPage = 100
			}
			if perPage < 1 {
				perPage = 1
			}
			page := jsParseIntOr(req.Query.Get("page"), 1)
			if page < 1 {
				page = 1
			}
			repos := listReposByOwner(store, viewerLogin)
			totalPages := (len(repos) + perPage - 1) / perPage
			if totalPages < 1 {
				totalPages = 1
			}
			start := (page - 1) * perPage
			end := page * perPage
			if start > len(repos) {
				start = len(repos)
			}
			if end > len(repos) {
				end = len(repos)
			}
			host := req.Headers["host"]
			if host == "" {
				host = "127.0.0.1"
			}
			base := "http://" + host + "/api/v3/user/repos"
			var links []string
			if page < totalPages {
				links = append(links, "<"+base+"?per_page="+intStr(perPage)+"&page="+intStr(page+1)+">; rel=\"next\"")
			}
			if page > 1 {
				links = append(links, "<"+base+"?per_page="+intStr(perPage)+"&page=1>; rel=\"first\"")
			}
			if totalPages > 1 {
				links = append(links, "<"+base+"?per_page="+intStr(perPage)+"&page="+intStr(totalPages)+">; rel=\"last\"")
			}
			var extra map[string]string
			if len(links) > 0 {
				extra = map[string]string{"Link": strings.Join(links, ", ")}
			}
			var items []*JVal
			for _, repo := range repos[start:end] {
				parsed, _ := ParseJSON(repoRestJSON(repo))
				items = append(items, parsed)
			}
			return restJSON(200, JArrVal(items...), extra)
		}
		if req.Method != "POST" {
			return restError(404, "Not Found")
		}
		parsed, malformed := parseJSONBody(req.Body)
		if malformed {
			return restError(400, "Problems parsing JSON")
		}
		name := strField(parsed, "name")
		if name == "" {
			return restError(422, "name is required")
		}
		private := false
		if v := parsed.ObjGet("private"); v != nil && v.Kind == JBool && v.Bool {
			private = true
		}
		repo := createRepo(store, viewerLogin, name, private, strField(parsed, "description"))
		if repo == nil {
			return restError(422, "name already exists on this account")
		}
		return restJSONBuffer(201, repoRestJSON(repo), nil)
	}

	// /api/v3/users/:login and /api/v3/users/:login/repos
	if first == "users" && second != "" {
		if second != viewerLogin {
			return restError(404, "Not Found: user "+second)
		}
		if len(segs) == 2 {
			if req.Method != "GET" {
				return restError(404, "Not Found")
			}
			return restJSON(200, userJSON(second), nil)
		}
		if third == "repos" && len(segs) == 3 {
			if req.Method != "GET" {
				return restError(404, "Not Found")
			}
			var items []*JVal
			for _, repo := range listReposByOwner(store, second) {
				parsed, _ := ParseJSON(repoRestJSON(repo))
				items = append(items, parsed)
			}
			return restJSON(200, JArrVal(items...), nil)
		}
		return restError(404, "Not Found")
	}

	// /api/v3/repos/:owner/:repo/...
	if first == "repos" && second != "" && third != "" {
		owner, repoName := second, third
		if foreign := ownerOr404(owner); foreign != nil {
			return *foreign
		}
		repo := getRepo(store, owner, repoName)
		tail := segs[3:]

		if len(tail) == 0 {
			if req.Method == "GET" {
				if repo == nil {
					return restError(404, "Not Found")
				}
				return restJSONBuffer(200, repoRestJSON(repo), nil)
			}
			if req.Method == "PATCH" {
				if repo == nil {
					return restError(404, "Not Found")
				}
				parsed, malformed := parseJSONBody(req.Body)
				if malformed {
					return restError(400, "Problems parsing JSON")
				}
				if v := parsed.ObjGet("description"); v != nil && v.Kind == JStr {
					repo.Description = v.Str
				}
				if v := parsed.ObjGet("default_branch"); v != nil && v.Kind == JStr {
					repo.DefaultBranch = v.Str
				}
				repo.RepoJSONCache = nil
				return restJSONBuffer(200, repoRestJSON(repo), nil)
			}
			if req.Method == "DELETE" {
				if repo == nil {
					return restError(404, "Not Found")
				}
				deleteRepo(store, owner, repoName)
				return restJSON(204, JNullVal(), nil)
			}
			return restError(404, "Not Found")
		}
		if repo == nil {
			return restError(404, "Not Found")
		}
		return handleRepoSub(store, repo, tail, req)
	}
	return restError(404, "Not Found")
}

// jsParseIntOr mirrors Number.parseInt(s, 10) || fallback.
func jsParseIntOr(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	v, ok := jsParseInt(s)
	if !ok || v == 0 {
		return fallback
	}
	return int(v)
}

func clampPage(v, fallback int) int {
	return v
}

func handleRepoSub(store *Store, repo *Repo, tail []string, req RestRequest) HttpResult {
	resource := tail[0]
	var a, b string
	if len(tail) > 1 {
		a = tail[1]
	}
	if len(tail) > 2 {
		b = tail[2]
	}
	hasA := len(tail) > 1
	hasB := len(tail) > 2

	if resource == "issues" {
		if !hasA {
			if req.Method == "GET" {
				state := "open"
				if req.Query.Has("state") {
					state = req.Query.Get("state")
				}
				sortBy := "created"
				if req.Query.Has("sort") {
					sortBy = req.Query.Get("sort")
				}
				direction := "desc"
				if req.Query.Has("direction") {
					direction = req.Query.Get("direction")
				}
				issues := listIssues(repo, state, sortBy, direction)
				var items []*JVal
				for _, issue := range issues {
					items = append(items, issueRestJSON(repo, issue))
				}
				return restJSON(200, JArrVal(items...), nil)
			}
			if req.Method == "POST" {
				parsed, malformed := parseJSONBody(req.Body)
				if malformed {
					return restError(400, "Problems parsing JSON")
				}
				title := strField(parsed, "title")
				if title == "" {
					return restError(422, "title is required")
				}
				var labels []string
				if v := parsed.ObjGet("labels"); v != nil && v.Kind == JArr {
					for _, item := range v.Arr {
						if item.Kind == JStr {
							labels = append(labels, item.Str)
						}
					}
				}
				issue, errMsg := createIssue(store, repo, title, strField(parsed, "body"), labels)
				if errMsg != "" {
					return restError(422, errMsg)
				}
				return restJSONBuffer(201, issueRestBuffer(repo, issue), nil)
			}
			return restError(404, "Not Found")
		}
		number, ok := jsParseInt(a)
		if !ok {
			return restError(404, "Not Found")
		}
		n := int(number)
		if b == "comments" {
			if req.Method == "GET" {
				comments := repo.Comments[n]
				var items []*JVal
				for _, c := range comments {
					items = append(items, JObjVal(
						JMember{"id", JIntVal(int64(c.ID))},
						JMember{"body", JStrVal(c.Body)},
						JMember{"created_at", JStrVal(c.CreatedAt)},
						JMember{"user", userJSON(viewerLogin)},
					))
				}
				return restJSON(200, JArrVal(items...), nil)
			}
			if req.Method == "POST" {
				parsed, malformed := parseJSONBody(req.Body)
				if malformed {
					return restError(400, "Problems parsing JSON")
				}
				comment := createComment(repo, n, strField(parsed, "body"))
				if comment == nil {
					return restError(404, "Not Found")
				}
				return restJSON(201, JObjVal(
					JMember{"id", JIntVal(int64(comment.ID))},
					JMember{"body", JStrVal(comment.Body)},
					JMember{"created_at", JStrVal(comment.CreatedAt)},
					JMember{"user", userJSON(viewerLogin)},
				), nil)
			}
			return restError(404, "Not Found")
		}
		if hasB {
			return restError(404, "Not Found")
		}
		if req.Method == "GET" {
			issue, ok := repo.Issues[n]
			if !ok {
				return restError(404, "Not Found")
			}
			return restJSONBuffer(200, issueRestBuffer(repo, issue), nil)
		}
		if req.Method == "PATCH" {
			parsed, malformed := parseJSONBody(req.Body)
			if malformed {
				return restError(400, "Problems parsing JSON")
			}
			var state, title, body *string
			if v := parsed.ObjGet("state"); v != nil && v.Kind == JStr {
				s := v.Str
				state = &s
			}
			if v := parsed.ObjGet("title"); v != nil && v.Kind == JStr {
				s := v.Str
				title = &s
			}
			if v := parsed.ObjGet("body"); v != nil && v.Kind == JStr {
				s := v.Str
				body = &s
			}
			issue := patchIssue(repo, n, state, title, body)
			if issue == nil {
				return restError(404, "Not Found")
			}
			return restJSONBuffer(200, issueRestBuffer(repo, issue), nil)
		}
		return restError(404, "Not Found")
	}

	if resource == "labels" {
		if hasA {
			return restError(404, "Not Found")
		}
		if req.Method == "GET" {
			var items []*JVal
			for _, name := range repo.LabelOrder {
				l := repo.Labels[name]
				items = append(items, JObjVal(
					JMember{"id", JIntVal(int64(l.ID))},
					JMember{"name", JStrVal(l.Name)},
					JMember{"color", JStrVal(l.Color)},
					JMember{"description", JStrVal(l.Description)},
				))
			}
			return restJSON(200, JArrVal(items...), nil)
		}
		if req.Method == "POST" {
			parsed, malformed := parseJSONBody(req.Body)
			if malformed {
				return restError(400, "Problems parsing JSON")
			}
			name := strField(parsed, "name")
			if name == "" {
				return restError(422, "name is required")
			}
			label := createLabel(repo, name, strField(parsed, "color"))
			return restJSON(201, JObjVal(
				JMember{"id", JIntVal(int64(label.ID))},
				JMember{"name", JStrVal(label.Name)},
				JMember{"color", JStrVal(label.Color)},
				JMember{"description", JStrVal(label.Description)},
			), nil)
		}
		return restError(404, "Not Found")
	}

	if resource == "pulls" {
		if !hasA {
			if req.Method == "GET" {
				state := req.Query.Get("state")
				if state == "" {
					state = "open"
				}
				var items []*JVal
				for _, p := range listPulls(repo, state) {
					items = append(items, pullRestJSON(repo, p))
				}
				return restJSON(200, JArrVal(items...), nil)
			}
			if req.Method == "POST" {
				parsed, malformed := parseJSONBody(req.Body)
				if malformed {
					return restError(400, "Problems parsing JSON")
				}
				head := strField(parsed, "head")
				base := strField(parsed, "base")
				title := strField(parsed, "title")
				if head == "" || base == "" || title == "" {
					return restError(422, "head, base and title are required")
				}
				pull, errMsg := createPull(repo, head, base, title, strField(parsed, "body"))
				if errMsg != "" {
					return restError(422, errMsg)
				}
				return restJSON(201, pullRestJSON(repo, pull), nil)
			}
			return restError(404, "Not Found")
		}
		number, ok := jsParseInt(a)
		if !ok {
			return restError(404, "Not Found")
		}
		n := int(number)
		pull, hasPull := repo.Pulls[n]
		if b == "files" {
			if req.Method != "GET" {
				return restError(404, "Not Found")
			}
			if !hasPull {
				return restError(404, "Not Found")
			}
			var items []*JVal
			for _, f := range pullFiles(repo, pull) {
				items = append(items, JObjVal(
					JMember{"filename", JStrVal(f.Filename)},
					JMember{"status", JStrVal(f.Status)},
					JMember{"additions", JIntVal(int64(f.Additions))},
					JMember{"deletions", JIntVal(int64(f.Deletions))},
					JMember{"changes", JIntVal(int64(f.Changes))},
				))
			}
			return restJSON(200, JArrVal(items...), nil)
		}
		if b == "merge" {
			if !hasPull {
				return restError(404, "Not Found")
			}
			if req.Method == "GET" {
				return restJSON(200, JObjVal(
					JMember{"mergeable", JBoolVal(pullMergeable(repo, pull))},
					JMember{"state", JStrVal(pull.State)},
					JMember{"merged", JBoolVal(pull.Merged)},
				), nil)
			}
			if req.Method == "PUT" {
				sha, errMsg, status := mergePull(repo, n)
				if errMsg != "" {
					return restError(status, errMsg)
				}
				return restJSON(200, JObjVal(
					JMember{"merged", JBoolVal(true)},
					JMember{"sha", JStrVal(sha)},
					JMember{"message", JStrVal("Pull Request successfully merged")},
				), nil)
			}
			return restError(404, "Not Found")
		}
		if hasB {
			return restError(404, "Not Found")
		}
		if req.Method == "GET" {
			if !hasPull {
				return restError(404, "Not Found")
			}
			return restJSON(200, pullRestJSON(repo, pull), nil)
		}
		return restError(404, "Not Found")
	}

	if resource == "branches" {
		if !hasA {
			if req.Method == "GET" {
				var items []*JVal
				var names []string
				for ref := range repo.Refs {
					if strings.HasPrefix(ref, "refs/heads/") {
						names = append(names, ref[len("refs/heads/"):])
					}
				}
				sortStrings(names)
				for _, name := range names {
					sha := repo.Refs["refs/heads/"+name]
					items = append(items, JObjVal(
						JMember{"name", JStrVal(name)},
						JMember{"commit", JObjVal(JMember{"sha", JStrVal(sha)})},
						JMember{"protected", JBoolVal(false)},
					))
				}
				return restJSON(200, JArrVal(items...), nil)
			}
			if req.Method == "POST" {
				parsed, malformed := parseJSONBody(req.Body)
				if malformed {
					return restError(400, "Problems parsing JSON")
				}
				branch := strField(parsed, "branch")
				sha := strField(parsed, "sha")
				if branch == "" || !isOid(sha) {
					return restError(422, "branch and a valid sha are required")
				}
				if _, exists := repo.Refs["refs/heads/"+branch]; exists {
					return restError(422, "branch already exists")
				}
				if getObject(repo, sha) == nil {
					return restError(422, "sha not found")
				}
				repo.Refs["refs/heads/"+branch] = sha
				repo.Version++
				return restJSON(201, JObjVal(
					JMember{"name", JStrVal(branch)},
					JMember{"commit", JObjVal(JMember{"sha", JStrVal(sha)})},
					JMember{"protected", JBoolVal(false)},
				), nil)
			}
			return restError(404, "Not Found")
		}
		return restError(404, "Not Found")
	}

	if resource == "commits" {
		if hasA {
			return restError(404, "Not Found")
		}
		if req.Method != "GET" {
			return restError(404, "Not Found")
		}
		sha := req.Query.Get("sha")
		if sha == "" {
			sha = repo.DefaultBranch
		}
		tip, ok := resolveRef(repo, sha)
		if !ok {
			if isOid(sha) {
				tip, ok = sha, true
			}
		}
		if !ok {
			return restError(404, "Not Found")
		}
		var items []*JVal
		for _, c := range listCommits(repo, tip) {
			items = append(items, JObjVal(
				JMember{"sha", JStrVal(c.Sha)},
				JMember{"commit", JObjVal(
					JMember{"message", JStrVal(c.Message)},
					JMember{"author", JObjVal(
						JMember{"name", JStrVal(viewerLogin)},
						JMember{"email", JStrVal(viewerEmail)},
					)},
					JMember{"committer", JObjVal(
						JMember{"name", JStrVal(viewerLogin)},
						JMember{"email", JStrVal(viewerEmail)},
					)},
				)},
				JMember{"html_url", JStrVal("https://github.com/" + repo.Owner + "/" + repo.Name + "/commit/" + c.Sha)},
				JMember{"url", JStrVal("/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/commits/" + c.Sha)},
			))
		}
		return restJSON(200, JArrVal(items...), nil)
	}

	if resource == "contents" {
		path, ok := decodeContentsPath(tail[1:])
		if !ok {
			return serverErrorResult()
		}
		if req.Method == "GET" {
			var ref *string
			if req.Query.Has("ref") {
				r := req.Query.Get("ref")
				ref = &r
			}
			result := readContent(repo, path, ref)
			if !result.Found {
				return restError(404, "Not Found")
			}
			if result.File != nil {
				file := result.File
				return restJSON(200, JObjVal(
					JMember{"type", JStrVal("file")},
					JMember{"name", JStrVal(file.Name)},
					JMember{"path", JStrVal(file.Path)},
					JMember{"sha", JStrVal(file.Sha)},
					JMember{"size", JIntVal(int64(file.Size))},
					JMember{"encoding", JStrVal("base64")},
					JMember{"content", JStrVal(b64Encode(file.Content))},
					JMember{"url", JStrVal("/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/contents/" + file.Path)},
					JMember{"html_url", JStrVal("https://github.com/" + repo.Owner + "/" + repo.Name + "/blob/" + repo.DefaultBranch + "/" + file.Path)},
					JMember{"download_url", JStrVal("https://raw.githubusercontent.com/" + repo.Owner + "/" + repo.Name + "/" + repo.DefaultBranch + "/" + file.Path)},
				), nil)
			}
			var items []*JVal
			for _, entry := range result.Dir {
				downloadURL := JNullVal()
				if entry.Type == "file" {
					downloadURL = JStrVal("https://raw.githubusercontent.com/" + repo.Owner + "/" + repo.Name + "/" + repo.DefaultBranch + "/" + entry.Path)
				}
				htmlKind := "blob"
				if entry.Type == "dir" {
					htmlKind = "tree"
				}
				items = append(items, JObjVal(
					JMember{"type", JStrVal(entry.Type)},
					JMember{"name", JStrVal(entry.Name)},
					JMember{"path", JStrVal(entry.Path)},
					JMember{"sha", JStrVal(entry.Sha)},
					JMember{"size", JIntVal(int64(entry.Size))},
					JMember{"url", JStrVal("/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/contents/" + entry.Path)},
					JMember{"html_url", JStrVal("https://github.com/" + repo.Owner + "/" + repo.Name + "/" + htmlKind + "/" + repo.DefaultBranch + "/" + entry.Path)},
					JMember{"download_url", downloadURL},
				))
			}
			return restJSON(200, JArrVal(items...), nil)
		}
		if req.Method == "PUT" {
			parsed, malformed := parseJSONBody(req.Body)
			if malformed {
				return restError(400, "Problems parsing JSON")
			}
			content := strField(parsed, "content")
			message := strField(parsed, "message")
			if content == "" || message == "" {
				return restError(422, "content and message are required")
			}
			var sha *string
			var hasSha bool
			if v := parsed.ObjGet("sha"); v != nil && v.Kind == JStr {
				s := v.Str
				sha, hasSha = &s, true
			}
			var branch *string
			var hasBranch bool
			if v := parsed.ObjGet("branch"); v != nil && v.Kind == JStr {
				s := v.Str
				branch, hasBranch = &s, true
			}
			contentSha, commitSha, errMsg, status := putContent(repo, path, content, message, strOr(sha), strOr(branch), hasSha, hasBranch)
			if errMsg != "" {
				return restError(status, errMsg)
			}
			name := path
			if i := strings.LastIndex(path, "/"); i >= 0 {
				name = path[i+1:]
			}
			return restJSON(201, JObjVal(
				JMember{"content", JObjVal(
					JMember{"type", JStrVal("file")},
					JMember{"name", JStrVal(name)},
					JMember{"path", JStrVal(path)},
					JMember{"sha", JStrVal(contentSha)},
					JMember{"size", JIntVal(int64(len(b64Decode(content))))},
					JMember{"encoding", JStrVal("base64")},
				)},
				JMember{"commit", JObjVal(
					JMember{"sha", JStrVal(commitSha)},
					JMember{"message", JStrVal(message)},
				)},
			), nil)
		}
		if req.Method == "DELETE" {
			parsed, malformed := parseJSONBody(req.Body)
			if malformed {
				return restError(400, "Problems parsing JSON")
			}
			sha := strField(parsed, "sha")
			message := strField(parsed, "message")
			if sha == "" || message == "" {
				return restError(422, "sha and message are required")
			}
			var branch *string
			var hasBranch bool
			if v := parsed.ObjGet("branch"); v != nil && v.Kind == JStr {
				s := v.Str
				branch, hasBranch = &s, true
			}
			commitSha, errMsg, status := deleteContent(repo, path, message, sha, strOr(branch), hasBranch)
			if errMsg != "" {
				return restError(status, errMsg)
			}
			return restJSON(200, JObjVal(
				JMember{"commit", JObjVal(
					JMember{"sha", JStrVal(commitSha)},
					JMember{"message", JStrVal(message)},
				)},
			), nil)
		}
		return restError(404, "Not Found")
	}

	if resource == "git" {
		if a == "refs" {
			refPath := strings.Join(tail[2:], "/")
			if req.Method == "GET" {
				if refPath == "" {
					var items []*JVal
					for _, r := range listRefs(repo) {
						items = append(items, JObjVal(
							JMember{"ref", JStrVal(r.Ref)},
							JMember{"object", JObjVal(
								JMember{"sha", JStrVal(r.Sha)},
								JMember{"type", JStrVal("commit")},
							)},
							JMember{"url", JStrVal("/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/refs/" + r.Ref)},
						))
					}
					return restJSON(200, JArrVal(items...), nil)
				}
				fullRef := "refs/" + refPath
				sha, ok := repo.Refs[fullRef]
				if !ok {
					return restError(404, "Not Found")
				}
				return restJSON(200, JObjVal(
					JMember{"ref", JStrVal(fullRef)},
					JMember{"object", JObjVal(
						JMember{"sha", JStrVal(sha)},
						JMember{"type", JStrVal("commit")},
					)},
					JMember{"url", JStrVal("/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/refs/" + fullRef)},
				), nil)
			}
			if req.Method == "POST" {
				parsed, malformed := parseJSONBody(req.Body)
				if malformed {
					return restError(400, "Problems parsing JSON")
				}
				ref := strField(parsed, "ref")
				sha := strField(parsed, "sha")
				if !strings.HasPrefix(ref, "refs/") || !isOid(sha) {
					return restError(422, "ref and a valid sha are required")
				}
				if _, exists := repo.Refs[ref]; exists {
					return restError(422, "reference already exists")
				}
				if getObject(repo, sha) == nil {
					return restError(422, "sha not found")
				}
				repo.Refs[ref] = sha
				repo.Version++
				return restJSON(201, JObjVal(
					JMember{"ref", JStrVal(ref)},
					JMember{"object", JObjVal(
						JMember{"sha", JStrVal(sha)},
						JMember{"type", JStrVal("commit")},
					)},
					JMember{"url", JStrVal("/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/refs/" + ref)},
				), nil)
			}
			return restError(404, "Not Found")
		}
		if a == "tags" {
			if hasB {
				if req.Method != "GET" {
					return restError(404, "Not Found")
				}
				object := getObject(repo, b)
				if object == nil || object.Type != "tag" {
					return restError(404, "Not Found")
				}
				tag := parseTag(object.Data)
				return restJSON(200, JObjVal(
					JMember{"sha", JStrVal(b)},
					JMember{"tag", JStrVal(tag.Tag)},
					JMember{"message", JStrVal(tag.Message)},
					JMember{"object", JObjVal(
						JMember{"sha", JStrVal(tag.Object)},
						JMember{"type", JStrVal(tag.Type)},
					)},
					JMember{"tagger", JObjVal(
						JMember{"name", JStrVal(viewerLogin)},
						JMember{"email", JStrVal(viewerEmail)},
					)},
				), nil)
			}
			if req.Method == "POST" {
				parsed, malformed := parseJSONBody(req.Body)
				if malformed {
					return restError(400, "Problems parsing JSON")
				}
				tagName := strField(parsed, "tag")
				message := strField(parsed, "message")
				objectSha := strField(parsed, "object")
				typ := strField(parsed, "type")
				if tagName == "" || !isOid(objectSha) || (typ != "commit" && typ != "tree" && typ != "blob" && typ != "tag") {
					return restError(422, "tag, message, object and type are required")
				}
				if getObject(repo, objectSha) == nil {
					return restError(422, "object not found")
				}
				identity := viewerLogin + " <" + viewerEmail + ">"
				data := buildTag(objectSha, typ, tagName, message, identity, tick())
				sha := hashObject("tag", data)
				if _, exists := repo.Objects[sha]; !exists {
					repo.Objects[sha] = &GitObject{Type: "tag", Data: data}
				}
				repo.Version++
				return restJSON(201, JObjVal(
					JMember{"sha", JStrVal(sha)},
					JMember{"tag", JStrVal(tagName)},
					JMember{"message", JStrVal(message)},
					JMember{"object", JObjVal(
						JMember{"sha", JStrVal(objectSha)},
						JMember{"type", JStrVal(typ)},
					)},
					JMember{"tagger", JObjVal(
						JMember{"name", JStrVal(viewerLogin)},
						JMember{"email", JStrVal(viewerEmail)},
					)},
				), nil)
			}
			return restError(404, "Not Found")
		}
		return restError(404, "Not Found")
	}

	return restError(404, "Not Found")
}

func strOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// decodeContentsPath decodes each path segment the way decodeURIComponent
// does; a malformed escape fails the whole request.
func decodeContentsPath(segments []string) (string, bool) {
	decoded := make([]string, len(segments))
	for i, s := range segments {
		d, err := url.PathUnescape(s)
		if err != nil {
			return "", false
		}
		decoded[i] = d
	}
	return strings.Join(decoded, "/"), true
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// serverErrorResult mirrors the server's catch-all 500.
func serverErrorResult() HttpResult {
	return HttpResult{Status: 500, Headers: map[string]string{"Content-Type": "application/json"}, Body: []byte(`{"message":"Internal Server Error"}`)}
}
