// MCP endpoint (JSON-RPC 2.0): server discovery, tools/list, tools/call,
// ping. The tools list is a frozen byte-exact JSON document.
package main

import (
	"strings"
)

const mcpServerName = "github-core-clone"
const mcpServerVersion = "1.0.0"
const mcpProtocolVersion = "2026-07-28"

var mcpSupportedVersions = []string{"2026-07-28", "2025-06-18", "2025-03-26", "2024-11-05"}

// The exact JSON.stringify({ tools: TOOLS }) output of the reference forge.
const mcpToolsListJSON = `{"tools":[{"name":"create_repo","description":"Create a new repository for the authenticated user.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner login"},"name":{"type":"string","description":"Repository name"},"description":{"type":"string","description":"Repository description"},"private":{"type":"boolean","description":"Whether the repository is private"}},"required":["owner","name"]}},{"name":"get_repo","description":"Get a repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"}},"required":["owner","repo"]}},{"name":"create_issue","description":"Create an issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},"labels":{"type":"array","items":{"type":"string"}}},"required":["owner","repo","title"]}},{"name":"list_issues","description":"List issues in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"state":{"type":"string","enum":["open","closed","all"]}},"required":["owner","repo"]}},{"name":"create_pull_request","description":"Create a pull request in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"head":{"type":"string"},"base":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"}},"required":["owner","repo","head","base","title"]}},{"name":"merge_pull_request","description":"Merge a pull request in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string"},"repo":{"type":"string"},"number":{"type":"integer"}},"required":["owner","repo","number"]}}]}`

var mcpToolRequired = map[string][]string{
	"create_repo":         {"owner", "name"},
	"get_repo":            {"owner", "repo"},
	"create_issue":        {"owner", "repo", "title"},
	"list_issues":         {"owner", "repo"},
	"create_pull_request": {"owner", "repo", "head", "base", "title"},
	"merge_pull_request":  {"owner", "repo", "number"},
}

func mcpRPCResult(id *JVal, result *JVal) *JVal {
	return JObjVal(
		JMember{"jsonrpc", JStrVal("2.0")},
		JMember{"id", id},
		JMember{"result", result},
	)
}

func mcpRPCError(id *JVal, code int, message string) *JVal {
	return JObjVal(
		JMember{"jsonrpc", JStrVal("2.0")},
		JMember{"id", id},
		JMember{"error", JObjVal(
			JMember{"code", JIntVal(int64(code))},
			JMember{"message", JStrVal(message)},
		)},
	)
}

func mcpToolText(value *JVal) *JVal {
	var text string
	if value.Kind == JStr {
		text = value.Str
	} else {
		text = string(value.Marshal())
	}
	var structured *JVal
	if value.Kind == JObj || value.Kind == JArr || value.Kind == JNull {
		structured = value
	} else {
		structured = JObjVal(JMember{"text", JStrVal(text)})
	}
	return JObjVal(
		JMember{"content", JArrVal(JObjVal(
			JMember{"type", JStrVal("text")},
			JMember{"text", JStrVal(text)},
		))},
		JMember{"structuredContent", structured},
	)
}

func mcpToolError(message string) *JVal {
	return JObjVal(
		JMember{"content", JArrVal(JObjVal(
			JMember{"type", JStrVal("text")},
			JMember{"text", JStrVal(message)},
		))},
		JMember{"isError", JBoolVal(true)},
	)
}

// mcpArg looks up a named argument; anything missing or non-object is
// undefined.
func mcpArg(args *JVal, name string) *JVal {
	if args == nil || args.Kind != JObj {
		return JUndefVal()
	}
	if v := args.ObjGet(name); v != nil {
		return v
	}
	return JUndefVal()
}

// mcpCoalesce mirrors (x ?? ''): null/undefined become the empty string,
// anything else is String(x).
func mcpCoalesce(v *JVal) string {
	if v == nil || v.Kind == JUndef || v.Kind == JNull {
		return ""
	}
	return jsStringify(v)
}

func mcpRequireArgs(args *JVal, names []string) string {
	for _, name := range names {
		v := mcpArg(args, name)
		if v.Kind == JUndef || v.Kind == JNull || (v.Kind == JStr && v.Str == "") {
			return "missing required argument: " + name
		}
	}
	return ""
}

func mcpCallTool(store *Store, name string, args *JVal) *JVal {
	if missing := mcpRequireArgs(args, mcpToolRequired[name]); missing != "" {
		return mcpToolError(missing)
	}
	owner := mcpCoalesce(mcpArg(args, "owner"))
	repoName := mcpCoalesce(mcpArg(args, "repo"))
	if v := mcpArg(args, "repo"); v.Kind == JUndef || v.Kind == JNull {
		repoName = mcpCoalesce(mcpArg(args, "name"))
	}

	switch name {
	case "create_repo":
		private := false
		if v := mcpArg(args, "private"); v.Kind == JBool && v.Bool {
			private = true
		}
		description := ""
		if v := mcpArg(args, "description"); v.Kind == JStr {
			description = v.Str
		}
		repo := createRepo(store, owner, mcpCoalesce(mcpArg(args, "name")), private, description)
		if repo == nil {
			return mcpToolError("repository " + owner + "/" + jsStringify(mcpArg(args, "name")) + " already exists")
		}
		parsed, _ := ParseJSON(repoRestJSON(repo))
		return mcpToolText(parsed)
	case "get_repo":
		repo := getRepo(store, owner, repoName)
		if repo == nil {
			return mcpToolError("repository " + owner + "/" + repoName + " not found")
		}
		parsed, _ := ParseJSON(repoRestJSON(repo))
		return mcpToolText(parsed)
	case "create_issue":
		repo := getRepo(store, owner, repoName)
		if repo == nil {
			return mcpToolError("repository " + owner + "/" + repoName + " not found")
		}
		var labels []string
		if v := mcpArg(args, "labels"); v.Kind == JArr {
			for _, item := range v.Arr {
				if item.Kind == JStr {
					labels = append(labels, item.Str)
				}
			}
		}
		issue, errMsg := createIssue(store, repo, mcpCoalesce(mcpArg(args, "title")), mcpCoalesce(mcpArg(args, "body")), labels)
		if errMsg != "" {
			return mcpToolError(errMsg)
		}
		return mcpToolText(issueRestJSON(repo, issue))
	case "list_issues":
		repo := getRepo(store, owner, repoName)
		if repo == nil {
			return mcpToolError("repository " + owner + "/" + repoName + " not found")
		}
		state := "open"
		if v := mcpArg(args, "state"); v.Kind == JStr {
			state = v.Str
		}
		issues := listIssues(repo, state, "created", "desc")
		var items []*JVal
		for _, issue := range issues {
			items = append(items, issueRestJSON(repo, issue))
		}
		return mcpToolText(JArrVal(items...))
	case "create_pull_request":
		repo := getRepo(store, owner, repoName)
		if repo == nil {
			return mcpToolError("repository " + owner + "/" + repoName + " not found")
		}
		pull, errMsg := createPull(repo, mcpCoalesce(mcpArg(args, "head")), mcpCoalesce(mcpArg(args, "base")), mcpCoalesce(mcpArg(args, "title")), mcpCoalesce(mcpArg(args, "body")))
		if errMsg != "" {
			return mcpToolError(errMsg)
		}
		return mcpToolText(pullRestJSON(repo, pull))
	case "merge_pull_request":
		repo := getRepo(store, owner, repoName)
		if repo == nil {
			return mcpToolError("repository " + owner + "/" + repoName + " not found")
		}
		n, _ := jsNumber(mcpArg(args, "number"))
		sha, errMsg, _ := mergePull(repo, int(n))
		if errMsg != "" {
			return mcpToolError(errMsg)
		}
		return mcpToolText(JObjVal(
			JMember{"merged", JBoolVal(true)},
			JMember{"sha", JStrVal(sha)},
			JMember{"message", JStrVal("Pull Request successfully merged")},
		))
	default:
		return mcpToolError("unknown tool: " + name)
	}
}

func handleMcp(store *Store, headers map[string]string, body []byte) HttpResult {
	if ok, _ := checkAuth(headers); !ok {
		return HttpResult{Status: 401, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: []byte(`{"message":"Requires authentication"}`)}
	}
	request, err := ParseJSON(body)
	if err != nil {
		return HttpResult{Status: 400, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)}
	}
	if request.Kind == JNull || request.Kind != JObj {
		resp := mcpRPCError(JNullVal(), -32600, "Invalid Request")
		return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: resp.Marshal()}
	}
	// The id: a string or number id is echoed; anything else becomes null.
	// A request with NO id member at all is a notification (202, no body).
	idMember := request.ObjGet("id")
	var id *JVal
	if idMember != nil && (idMember.Kind == JStr || idMember.Kind == JNum) {
		id = idMember
	} else {
		id = JNullVal()
	}
	if idMember == nil {
		return HttpResult{Status: 202, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: []byte{}}
	}
	method := ""
	if v := request.ObjGet("method"); v != nil && v.Kind == JStr {
		method = v.Str
	}
	params := &JVal{Kind: JObj}
	if v := request.ObjGet("params"); v != nil {
		params = v
	}

	switch method {
	case "server/discover":
		requested := mcpProtocolVersion
		if v := mcpArg(params, "protocolVersion"); v.Kind == JStr {
			requested = v.Str
		}
		version := mcpProtocolVersion
		for _, sv := range mcpSupportedVersions {
			if sv == requested {
				version = requested
				break
			}
		}
		var versions []*JVal
		for _, sv := range mcpSupportedVersions {
			versions = append(versions, JStrVal(sv))
		}
		resp := mcpRPCResult(id, JObjVal(
			JMember{"protocolVersion", JStrVal(version)},
			JMember{"supportedVersions", JArrVal(versions...)},
			JMember{"capabilities", JObjVal(JMember{"tools", JObjVal()})},
			JMember{"serverInfo", JObjVal(
				JMember{"name", JStrVal(mcpServerName)},
				JMember{"version", JStrVal(mcpServerVersion)},
			)},
		))
		return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: resp.Marshal()}
	case "initialize":
		requested := mcpProtocolVersion
		if v := mcpArg(params, "protocolVersion"); v.Kind == JStr {
			requested = v.Str
		}
		version := mcpProtocolVersion
		for _, sv := range mcpSupportedVersions {
			if sv == requested {
				version = requested
				break
			}
		}
		resp := mcpRPCResult(id, JObjVal(
			JMember{"protocolVersion", JStrVal(version)},
			JMember{"capabilities", JObjVal(JMember{"tools", JObjVal()})},
			JMember{"serverInfo", JObjVal(
				JMember{"name", JStrVal(mcpServerName)},
				JMember{"version", JStrVal(mcpServerVersion)},
			)},
		))
		return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: resp.Marshal()}
	case "tools/list":
		// The frozen list, embedded verbatim.
		var b strings.Builder
		b.WriteString(`{"jsonrpc":"2.0","id":`)
		b.Write(id.Marshal())
		b.WriteString(`,"result":`)
		b.WriteString(mcpToolsListJSON)
		b.WriteString(`}`)
		return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: []byte(b.String())}
	case "tools/call":
		toolName := ""
		if v := mcpArg(params, "name"); v.Kind == JStr {
			toolName = v.Str
		}
		args := &JVal{Kind: JObj}
		if v := params.ObjGet("arguments"); v != nil {
			args = v
		}
		if _, known := mcpToolRequired[toolName]; !known {
			resp := mcpRPCError(id, -32601, "unknown tool: "+toolName)
			return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: resp.Marshal()}
		}
		resp := mcpRPCResult(id, mcpCallTool(store, toolName, args))
		return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: resp.Marshal()}
	case "ping":
		resp := mcpRPCResult(id, JObjVal())
		return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: resp.Marshal()}
	default:
		resp := mcpRPCError(id, -32601, "method not found: "+method)
		return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: resp.Marshal()}
	}
}
