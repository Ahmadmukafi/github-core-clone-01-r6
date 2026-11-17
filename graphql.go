// GraphQL endpoint: a tokenizer + parser for the harness's subset, and an
// executor for Query/Mutation over the forge's store. Responses are built to
// be byte-identical to the reference forge's.
package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// --- tokenizer ---

type gqlToken struct {
	kind  string // name | int | float | string | punct | eof
	value string
}

func tokenizeGQL(source string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	n := len(source)
	for i < n {
		ch := source[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if ch == '#' {
			for i < n && source[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '"' {
			if strings.HasPrefix(source[i:], `"""`) {
				end := strings.Index(source[i+3:], `"""`)
				var raw string
				if end < 0 {
					raw = source[i+3:]
					i = n
				} else {
					raw = source[i+3 : i+3+end]
					i = i + 3 + end + 3
				}
				tokens = append(tokens, gqlToken{kind: "string", value: raw})
				continue
			}
			j := i + 1
			var out strings.Builder
			for j < n && source[j] != '"' {
				if source[j] == '\\' && j+1 < n {
					esc := source[j+1]
					switch esc {
					case 'n':
						out.WriteByte('\n')
					case 't':
						out.WriteByte('\t')
					case 'r':
						out.WriteByte('\r')
					case '"':
						out.WriteByte('"')
					case '\\':
						out.WriteByte('\\')
					case '/':
						out.WriteByte('/')
					case 'u':
						if j+5 < n {
							if cp, err := strconv.ParseInt(source[j+2:j+6], 16, 32); err == nil {
								out.WriteRune(rune(cp))
							} else {
								out.WriteRune(0)
							}
						} else {
							out.WriteByte('u')
						}
						j += 6
						continue
					default:
						out.WriteByte(esc)
					}
					j += 2
					continue
				}
				out.WriteByte(source[j])
				j++
			}
			tokens = append(tokens, gqlToken{kind: "string", value: out.String()})
			i = j + 1
			continue
		}
		if ch == '.' && strings.HasPrefix(source[i:], "...") {
			tokens = append(tokens, gqlToken{kind: "punct", value: "..."})
			i += 3
			continue
		}
		if strings.IndexByte("!$():=@[]{}|&", ch) >= 0 {
			tokens = append(tokens, gqlToken{kind: "punct", value: string(ch)})
			i++
			continue
		}
		if (ch >= '0' && ch <= '9') || ch == '-' {
			j := i
			if source[j] == '-' {
				j++
			}
			isFloat := false
			for j < n && source[j] >= '0' && source[j] <= '9' {
				j++
			}
			if j < n && source[j] == '.' {
				isFloat = true
				j++
				for j < n && source[j] >= '0' && source[j] <= '9' {
					j++
				}
			}
			if j < n && (source[j] == 'e' || source[j] == 'E') {
				isFloat = true
				j++
				if j < n && (source[j] == '+' || source[j] == '-') {
					j++
				}
				for j < n && source[j] >= '0' && source[j] <= '9' {
					j++
				}
			}
			kind := "int"
			if isFloat {
				kind = "float"
			}
			tokens = append(tokens, gqlToken{kind: kind, value: source[i:j]})
			i = j
			continue
		}
		if ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') {
			j := i
			for j < n && (source[j] == '_' || (source[j] >= 'A' && source[j] <= 'Z') || (source[j] >= 'a' && source[j] <= 'z') || (source[j] >= '0' && source[j] <= '9')) {
				j++
			}
			tokens = append(tokens, gqlToken{kind: "name", value: source[i:j]})
			i = j
			continue
		}
		return nil, fmt.Errorf("unexpected character %c at %d", ch, i)
	}
	tokens = append(tokens, gqlToken{kind: "eof"})
	return tokens, nil
}

// jsParseFloat mirrors Number.parseFloat: it parses the longest valid prefix.
func jsParseFloatToken(s string) float64 {
	// The tokenizer only yields well-formed prefixes except a trailing
	// exponent marker with no digits; drop that marker.
	for len(s) > 0 {
		last := s[len(s)-1]
		if last == 'e' || last == 'E' || last == '+' || last == '-' {
			// Only a trailing exponent sign is invalid; a lone sign is not a
			// float token in practice.
			if last == 'e' || last == 'E' {
				s = s[:len(s)-1]
				continue
			}
			if len(s) >= 2 && (s[len(s)-2] == 'e' || s[len(s)-2] == 'E') {
				s = s[:len(s)-2]
				continue
			}
		}
		break
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// --- AST ---

type gqlValue struct {
	kind   string // variable | int | float | string | boolean | null | enum | list | object
	name   string
	num    float64
	str    string
	boolean bool
	values []*gqlValue
	fields []gqlField
}

type gqlField struct {
	name  string
	value *gqlValue
}

type gqlSelection struct {
	kind          string // field | fragmentSpread | inlineFragment
	name          string
	alias         string
	hasAlias      bool
	args          []gqlField
	typeCondition string
	hasTypeCond   bool
	fragmentName  string
	selections    []*gqlSelection
}

type gqlOperation struct {
	kind      string // query | mutation
	name      string
	hasName   bool
	selections []*gqlSelection
}

type gqlFragment struct {
	name          string
	typeCondition string
	selections    []*gqlSelection
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.pos] }

func (p *gqlParser) next() gqlToken {
	t := p.tokens[p.pos]
	p.pos++
	return t
}

func (p *gqlParser) expectPunct(value string) error {
	t := p.next()
	if t.kind != "punct" || t.value != value {
		return fmt.Errorf("expected %s, got %s", value, t.kind)
	}
	return nil
}

func (p *gqlParser) expectName(value ...string) (string, error) {
	t := p.next()
	if t.kind != "name" {
		return "", fmt.Errorf("expected name, got %s", t.kind)
	}
	if len(value) > 0 && t.value != value[0] {
		return "", fmt.Errorf("expected %s, got %s", value[0], t.value)
	}
	return t.value, nil
}

func (p *gqlParser) skipPunct(value string) bool {
	t := p.peek()
	if t.kind == "punct" && t.value == value {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) parseDocument() ([]*gqlOperation, map[string]*gqlFragment, error) {
	var operations []*gqlOperation
	fragments := map[string]*gqlFragment{}
	for p.peek().kind != "eof" {
		t := p.peek()
		if t.kind == "name" && t.value == "fragment" {
			p.next()
			name, err := p.expectName()
			if err != nil {
				return nil, nil, err
			}
			if _, err := p.expectName("on"); err != nil {
				return nil, nil, err
			}
			typeCond, err := p.expectName()
			if err != nil {
				return nil, nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, nil, err
			}
			fragments[name] = &gqlFragment{name: name, typeCondition: typeCond, selections: sels}
			continue
		}
		op, err := p.parseOperation()
		if err != nil {
			return nil, nil, err
		}
		operations = append(operations, op)
	}
	return operations, fragments, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	kind := "query"
	t := p.peek()
	if t.kind == "name" && (t.value == "query" || t.value == "mutation") {
		kind = p.next().value
	}
	op := &gqlOperation{kind: kind}
	if next := p.peek(); next.kind == "name" {
		op.name = p.next().value
		op.hasName = true
	}
	if p.skipPunct("(") {
		for !p.skipPunct(")") {
			if err := p.expectPunct("$"); err != nil {
				return nil, err
			}
			if _, err := p.expectName(); err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			if err := p.parseType(); err != nil {
				return nil, err
			}
			if p.skipPunct("=") {
				if _, err := p.parseValue(); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := p.parseDirectives(); err != nil {
		return nil, err
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selections = sels
	return op, nil
}

func (p *gqlParser) parseType() error {
	if p.skipPunct("[") {
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(); err != nil {
			return err
		}
	}
	p.skipPunct("!")
	return nil
}

func (p *gqlParser) parseDirectives() error {
	for p.skipPunct("@") {
		if _, err := p.expectName(); err != nil {
			return err
		}
		if p.skipPunct("(") {
			for !p.skipPunct(")") {
				if _, err := p.expectName(); err != nil {
					return err
				}
				if err := p.expectPunct(":"); err != nil {
					return err
				}
				if _, err := p.parseValue(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	for !p.skipPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	if p.peek().kind == "punct" && p.peek().value == "..." {
		p.next()
		t := p.peek()
		if t.kind == "name" && t.value == "on" {
			p.next()
			typeCond, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.parseDirectives(); err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{kind: "inlineFragment", typeCondition: typeCond, hasTypeCond: true, selections: sels}, nil
		}
		if t.kind == "name" {
			fragName := p.next().value
			if err := p.parseDirectives(); err != nil {
				return nil, err
			}
			return &gqlSelection{kind: "fragmentSpread", fragmentName: fragName}, nil
		}
		if err := p.parseDirectives(); err != nil {
			return nil, err
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &gqlSelection{kind: "inlineFragment", selections: sels}, nil
	}
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	sel := &gqlSelection{kind: "field", name: name}
	if p.peek().kind == "punct" && p.peek().value == ":" {
		p.next()
		sel.alias = name
		sel.hasAlias = true
		name2, err := p.expectName()
		if err != nil {
			return nil, err
		}
		sel.name = name2
	}
	if p.skipPunct("(") {
		for !p.skipPunct(")") {
			argName, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			sel.args = append(sel.args, gqlField{name: argName, value: val})
		}
	}
	if err := p.parseDirectives(); err != nil {
		return nil, err
	}
	if p.peek().kind == "punct" && p.peek().value == "{" {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selections = sels
	}
	return sel, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t := p.next()
	if t.kind == "punct" && t.value == "$" {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "variable", name: name}, nil
	}
	if t.kind == "punct" && t.value == "[" {
		v := &gqlValue{kind: "list"}
		for !p.skipPunct("]") {
			item, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.values = append(v.values, item)
		}
		return v, nil
	}
	if t.kind == "punct" && t.value == "{" {
		v := &gqlValue{kind: "object"}
		for !p.skipPunct("}") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.fields = append(v.fields, gqlField{name: name, value: val})
		}
		return v, nil
	}
	if t.kind == "int" {
		n, ok := jsParseInt(t.value)
		if !ok {
			n = 0
			return &gqlValue{kind: "int", num: math.NaN()}, nil
		}
		return &gqlValue{kind: "int", num: float64(n)}, nil
	}
	if t.kind == "float" {
		return &gqlValue{kind: "float", num: jsParseFloatToken(t.value)}, nil
	}
	if t.kind == "string" {
		return &gqlValue{kind: "string", str: t.value}, nil
	}
	if t.kind == "name" {
		if t.value == "true" {
			return &gqlValue{kind: "boolean", boolean: true}, nil
		}
		if t.value == "false" {
			return &gqlValue{kind: "boolean", boolean: false}, nil
		}
		if t.value == "null" {
			return &gqlValue{kind: "null"}, nil
		}
		return &gqlValue{kind: "enum", str: t.value}, nil
	}
	return nil, fmt.Errorf("unexpected value token %s", t.kind)
}

// --- value resolution ---

func resolveGQLValue(node *gqlValue, variables map[string]*JVal) *JVal {
	switch node.kind {
	case "variable":
		if v, ok := variables[node.name]; ok {
			return v
		}
		return JUndefVal()
	case "int", "float":
		return JNumVal(node.num)
	case "string":
		return JStrVal(node.str)
	case "boolean":
		return JBoolVal(node.boolean)
	case "null":
		return JNullVal()
	case "enum":
		return JStrVal(node.str)
	case "list":
		items := make([]*JVal, 0, len(node.values))
		for _, v := range node.values {
			items = append(items, resolveGQLValue(v, variables))
		}
		return JArrVal(items...)
	case "object":
		out := &JVal{Kind: JObj}
		for _, f := range node.fields {
			out.ObjSet(f.name, resolveGQLValue(f.value, variables))
		}
		return out
	}
	return JUndefVal()
}

func resolveGQLArgs(sel *gqlSelection, variables map[string]*JVal) map[string]*JVal {
	out := map[string]*JVal{}
	for _, arg := range sel.args {
		out[arg.name] = resolveGQLValue(arg.value, variables)
	}
	return out
}

// --- type metadata ---

var gqlTypeFields = map[string][]string{
	"Query":           {"viewer", "repository", "node", "search"},
	"Mutation":        {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"Issue":           {"id", "number", "title", "body", "state", "labels", "comments", "repository", "author", "createdAt", "updatedAt", "url"},
	"PullRequest":     {"id", "number", "title", "body", "state", "headRefName", "baseRefName", "headRef", "baseRef", "merged", "mergeable", "files", "createdAt", "updatedAt", "url"},
	"Repository":      {"id", "databaseId", "name", "nameWithOwner", "description", "owner", "defaultBranchRef", "issues", "issue", "pullRequests", "pullRequest", "labels", "viewerPermission", "hasIssuesEnabled", "url", "createdAt"},
	"User":            {"id", "login", "avatarUrl", "url"},
	"Label":           {"id", "name", "color", "description"},
	"IssueComment":    {"id", "body", "author", "createdAt"},
	"Ref":             {"name", "target"},
	"GitObject":       {"oid"},
	"PullRequestFile": {"path", "additions", "deletions", "changeType", "status"},
}

var gqlTypeKinds = map[string]string{
	"Issue": "OBJECT", "PullRequest": "OBJECT", "Repository": "OBJECT",
	"Query": "OBJECT", "Mutation": "OBJECT", "User": "OBJECT",
	"Label": "OBJECT", "IssueComment": "OBJECT", "Ref": "OBJECT",
	"GitObject": "OBJECT", "SearchType": "ENUM", "IssueState": "ENUM",
	"PullRequestState": "ENUM",
}

// Insertion order of TYPE_KINDS (Object.keys order in the reference).
var gqlTypeKindOrder = []string{
	"Issue", "PullRequest", "Repository", "Query", "Mutation", "User",
	"Label", "IssueComment", "Ref", "GitObject", "SearchType",
	"IssueState", "PullRequestState",
}

var gqlEnumValues = map[string][]string{
	"SearchType":       {"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "MILESTONE", "LABEL", "CODE", "WIKI"},
	"IssueState":       {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
}

// --- executor ---

// GqlObject is a node in the GraphQL result tree. Fields is an ordered
// member list; Repo/Issue/Pull are the backing store records.
type GqlObject struct {
	Type   string
	Fields []JMember
	Repo   *Repo
	Issue  *Issue
	Pull   *Pull
}

func newGqlObject(typ string) *GqlObject {
	return &GqlObject{Type: typ}
}

func (o *GqlObject) set(key string, val *JVal) *GqlObject {
	for i := range o.Fields {
		if o.Fields[i].Key == key {
			o.Fields[i].Val = val
			return o
		}
	}
	o.Fields = append(o.Fields, JMember{Key: key, Val: val})
	return o
}

func (o *GqlObject) get(key string) (*JVal, bool) {
	for i := range o.Fields {
		if o.Fields[i].Key == key {
			return o.Fields[i].Val, true
		}
	}
	return nil, false
}

// gqlObjectJSON renders a GqlObject the way JSON.stringify would render the
// reference's plain object: __type first, then its public fields.
func gqlObjectJSON(o *GqlObject) *JVal {
	out := &JVal{Kind: JObj}
	out.ObjSet("__type", JStrVal(o.Type))
	for _, m := range o.Fields {
		out.ObjSet(m.Key, m.Val)
	}
	return out
}

type gqlExecutor struct {
	store     *Store
	variables map[string]*JVal
	errors    []*JVal
	fragments map[string]*gqlFragment
}

func (e *gqlExecutor) pushError(msg string) {
	e.errors = append(e.errors, JObjVal(JMember{"message", JStrVal(msg)}))
}

func cursorFor(index int) string {
	return b64Encode([]byte("cursor:" + intStr(index)))
}

func indexFromCursor(cursor string) int {
	if cursor == "" {
		return -1
	}
	decoded := string(b64Decode(cursor))
	if !strings.HasPrefix(decoded, "cursor:") {
		return -1
	}
	rest := decoded[len("cursor:"):]
	if rest == "" {
		return -1
	}
	for i := 0; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return -1
		}
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return -1
	}
	return int(n)
}

type gqlEdge struct {
	Cursor string
	Node   *GqlObject
}

type gqlConnection struct {
	TotalCount      int
	Edges           []gqlEdge
	Nodes           []*GqlObject
	HasNextPage     bool
	HasPreviousPage bool
	StartCursor     *string
	EndCursor       *string
}

func buildGQLConnection(items []*GqlObject, args map[string]*JVal) *gqlConnection {
	var after string
	if v, ok := args["after"]; ok && v != nil && v.Kind == JStr {
		after = v.Str
	}
	var first *float64
	if v, ok := args["first"]; ok && v != nil && v.Kind == JNum {
		f := v.Num
		first = &f
	}
	start := indexFromCursor(after) + 1
	end := len(items)
	if first != nil {
		switch {
		case math.IsNaN(*first):
			end = 0
		case math.IsInf(*first, 1):
			end = len(items)
		case math.IsInf(*first, -1):
			end = 0
		default:
			if cand := int(float64(start) + *first); cand < end {
				end = cand
			}
		}
	}
	if start < 0 {
		start = 0
	}
	if start > len(items) {
		start = len(items)
	}
	if end < start {
		end = start
	}
	if end > len(items) {
		end = len(items)
	}
	slice := items[start:end]
	conn := &gqlConnection{
		TotalCount:      len(items),
		HasNextPage:     end < len(items),
		HasPreviousPage: start > 0,
	}
	for i, item := range slice {
		conn.Edges = append(conn.Edges, gqlEdge{Cursor: cursorFor(start + i), Node: item})
		conn.Nodes = append(conn.Nodes, item)
	}
	if len(slice) > 0 {
		sc := cursorFor(start)
		ec := cursorFor(end - 1)
		conn.StartCursor = &sc
		conn.EndCursor = &ec
	}
	return conn
}

func issueGQLObject(repo *Repo, issue *Issue) *GqlObject {
	o := newGqlObject("Issue")
	o.Repo = repo
	o.Issue = issue
	o.set("id", JStrVal(globalId("Issue", intStr(repo.ID), intStr(issue.Number))))
	o.set("number", JIntVal(int64(issue.Number)))
	o.set("title", JStrVal(issue.Title))
	o.set("body", JStrVal(issue.Body))
	o.set("state", JStrVal(strings.ToUpper(issue.State)))
	o.set("createdAt", JStrVal(issue.CreatedAt))
	o.set("updatedAt", JStrVal(issue.UpdatedAt))
	o.set("url", JStrVal("https://github.com/"+repo.Owner+"/"+repo.Name+"/issues/"+intStr(issue.Number)))
	return o
}

func pullGQLObject(repo *Repo, pull *Pull) *GqlObject {
	o := newGqlObject("PullRequest")
	o.Repo = repo
	o.Pull = pull
	state := "CLOSED"
	if pull.State == "open" {
		state = "OPEN"
	} else if pull.Merged {
		state = "MERGED"
	}
	o.set("id", JStrVal(globalId("PullRequest", intStr(repo.ID), intStr(pull.Number))))
	o.set("number", JIntVal(int64(pull.Number)))
	o.set("title", JStrVal(pull.Title))
	o.set("body", JStrVal(pull.Body))
	o.set("state", JStrVal(state))
	o.set("headRefName", JStrVal(pull.HeadRef))
	o.set("baseRefName", JStrVal(pull.BaseRef))
	o.set("merged", JBoolVal(pull.Merged))
	o.set("createdAt", JStrVal(pull.CreatedAt))
	o.set("updatedAt", JStrVal(pull.UpdatedAt))
	o.set("url", JStrVal("https://github.com/"+repo.Owner+"/"+repo.Name+"/pull/"+intStr(pull.Number)))
	return o
}

func repoGQLObject(repo *Repo) *GqlObject {
	o := newGqlObject("Repository")
	o.Repo = repo
	o.set("id", JStrVal(globalId("Repository", intStr(repo.ID))))
	o.set("databaseId", JIntVal(int64(repo.ID)))
	o.set("name", JStrVal(repo.Name))
	o.set("nameWithOwner", JStrVal(repo.Owner+"/"+repo.Name))
	o.set("description", JStrVal(repo.Description))
	o.set("url", JStrVal("https://github.com/"+repo.Owner+"/"+repo.Name))
	o.set("createdAt", JStrVal(repo.CreatedAt))
	return o
}

func userGQLObject(login string) *GqlObject {
	id := 2000
	if login == viewerLogin {
		id = 1000
	}
	o := newGqlObject("User")
	o.set("id", JStrVal(globalId("User", intStr(id))))
	o.set("login", JStrVal(login))
	o.set("avatarUrl", JStrVal("https://github.com/"+login+".png"))
	o.set("url", JStrVal("https://github.com/"+login))
	return o
}

func labelGQLObject(repo *Repo, label *Label) *GqlObject {
	o := newGqlObject("Label")
	o.set("id", JStrVal(globalId("Label", intStr(repo.ID), intStr(label.ID))))
	o.set("name", JStrVal(label.Name))
	o.set("color", JStrVal(label.Color))
	o.set("description", JStrVal(label.Description))
	return o
}

func findByGlobalID(store *Store, id string) *GqlObject {
	decoded := string(b64Decode(id))
	parts := strings.Split(decoded, ":")
	if len(parts) == 0 {
		return nil
	}
	kind := parts[0]
	if kind == "Repository" && len(parts) == 2 {
		repoID, ok := jsParseInt(parts[1])
		if !ok {
			return nil
		}
		for _, repo := range store.Repos {
			if int64(repo.ID) == repoID {
				return repoGQLObject(repo)
			}
		}
		return nil
	}
	if (kind == "Issue" || kind == "PullRequest") && len(parts) == 3 {
		repoID, ok1 := jsParseInt(parts[1])
		number, ok2 := jsParseInt(parts[2])
		if !ok1 || !ok2 {
			return nil
		}
		for _, repo := range store.Repos {
			if int64(repo.ID) != repoID {
				continue
			}
			if kind == "Issue" {
				issue, ok := repo.Issues[int(number)]
				if !ok {
					return nil
				}
				return issueGQLObject(repo, issue)
			}
			pull, ok := repo.Pulls[int(number)]
			if !ok {
				return nil
			}
			return pullGQLObject(repo, pull)
		}
		return nil
	}
	if kind == "User" {
		return userGQLObject(viewerLogin)
	}
	return nil
}

func (e *gqlExecutor) executeSelections(object *GqlObject, selections []*gqlSelection, out *JVal) {
	for _, sel := range selections {
		if sel.kind == "fragmentSpread" {
			frag, ok := e.fragments[sel.fragmentName]
			if ok {
				e.executeSelections(object, frag.selections, out)
			}
			continue
		}
		if sel.kind == "inlineFragment" {
			if sel.hasTypeCond && sel.typeCondition != object.Type {
				continue
			}
			e.executeSelections(object, sel.selections, out)
			continue
		}
		key := sel.name
		if sel.hasAlias {
			key = sel.alias
		}
		args := resolveGQLArgs(sel, e.variables)
		result := e.resolveField(object, sel.name, args, sel.selections)
		out.ObjSet(key, result)
	}
}

func (e *gqlExecutor) resolveField(object *GqlObject, field string, args map[string]*JVal, selections []*gqlSelection) *JVal {
	typ := object.Type
	if field == "__typename" {
		return JStrVal(typ)
	}

	if typ == "Query" {
		switch field {
		case "viewer":
			return e.selectObject(userGQLObject(viewerLogin), selections)
		case "repository":
			repo := getRepo(e.store, jsStringify(argOrUndef(args, "owner")), jsStringify(argOrUndef(args, "name")))
			if repo == nil {
				return JNullVal()
			}
			return e.selectObject(repoGQLObject(repo), selections)
		case "node":
			found := findByGlobalID(e.store, jsStringify(argOrUndef(args, "id")))
			if found == nil {
				return JNullVal()
			}
			return e.selectObject(found, selections)
		case "__type":
			name := jsStringify(argOrUndef(args, "name"))
			kind, ok := gqlTypeKinds[name]
			if !ok {
				return JNullVal()
			}
			typeObj := newGqlObject("__Type")
			typeObj.set("name", JStrVal(name))
			typeObj.set("kind", JStrVal(kind))
			var fieldObjs []*GqlObject
			for _, f := range gqlTypeFields[name] {
				fo := newGqlObject("__Field")
				fo.set("name", JStrVal(f))
				fieldObjs = append(fieldObjs, fo)
			}
			typeObj.set("fields", JArrRefVal(fieldObjs))
			var enumObjs []*GqlObject
			for _, v := range gqlEnumValues[name] {
				eo := newGqlObject("__EnumValue")
				eo.set("name", JStrVal(v))
				enumObjs = append(enumObjs, eo)
			}
			typeObj.set("enumValues", JArrRefVal(enumObjs))
			return e.selectObject(typeObj, selections)
		case "__schema":
			schemaObj := newGqlObject("__Schema")
			qt := newGqlObject("__Type")
			qt.set("name", JStrVal("Query"))
			schemaObj.set("queryType", JObjRefVal(qt))
			mt := newGqlObject("__Type")
			mt.set("name", JStrVal("Mutation"))
			schemaObj.set("mutationType", JObjRefVal(mt))
			var typeObjs []*GqlObject
			for _, name := range gqlTypeKindOrder {
				to := newGqlObject("__Type")
				to.set("name", JStrVal(name))
				to.set("kind", JStrVal(gqlTypeKinds[name]))
				typeObjs = append(typeObjs, to)
			}
			schemaObj.set("types", JArrRefVal(typeObjs))
			return e.selectObject(schemaObj, selections)
		}
		e.pushError(fmt.Sprintf("Cannot query field \"%s\" on type \"Query\".", field))
		return JNullVal()
	}

	if typ == "Mutation" {
		return e.resolveMutation(field, args, selections)
	}

	if typ == "Repository" {
		repo := object.Repo
		switch field {
		case "issue":
			n, ok := gqlNumberArg(args, "number")
			if !ok {
				return JNullVal()
			}
			issue, exists := repo.Issues[int(n)]
			if !exists {
				return JNullVal()
			}
			return e.selectObject(issueGQLObject(repo, issue), selections)
		case "pullRequest":
			n, ok := gqlNumberArg(args, "number")
			if !ok {
				return JNullVal()
			}
			pull, exists := repo.Pulls[int(n)]
			if !exists {
				return JNullVal()
			}
			return e.selectObject(pullGQLObject(repo, pull), selections)
		case "viewerPermission":
			return JStrVal("ADMIN")
		case "hasIssuesEnabled":
			return JBoolVal(true)
		case "issues":
			state := gqlStatesArg(args, "states")
			issues := listIssues(repo, state, "created", "desc")
			var items []*GqlObject
			for _, issue := range issues {
				items = append(items, issueGQLObject(repo, issue))
			}
			return e.selectConnection(buildGQLConnection(items, args), selections)
		case "pullRequests":
			state := gqlStatesArg(args, "states")
			pulls := listPulls(repo, state)
			var items []*GqlObject
			for _, pull := range pulls {
				items = append(items, pullGQLObject(repo, pull))
			}
			return e.selectConnection(buildGQLConnection(items, args), selections)
		case "labels":
			var items []*GqlObject
			for _, name := range repo.LabelOrder {
				items = append(items, labelGQLObject(repo, repo.Labels[name]))
			}
			return e.selectConnection(buildGQLConnection(items, args), selections)
		case "defaultBranchRef":
			sha, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
			if !ok {
				return JNullVal()
			}
			refObj := newGqlObject("Ref")
			refObj.set("name", JStrVal(repo.DefaultBranch))
			target := newGqlObject("GitObject")
			target.set("oid", JStrVal(sha))
			refObj.set("target", JObjRefVal(target))
			return e.selectObject(refObj, selections)
		case "owner":
			return e.selectObject(userGQLObject(repo.Owner), selections)
		}
		if containsStr([]string{"id", "databaseId", "name", "nameWithOwner", "description", "url", "createdAt"}, field) {
			if v, ok := object.get(field); ok {
				return v
			}
		}
		e.pushError(fmt.Sprintf("Cannot query field \"%s\" on type \"Repository\".", field))
		return JNullVal()
	}

	if typ == "Issue" {
		repo := object.Repo
		issue := object.Issue
		switch field {
		case "labels":
			var items []*GqlObject
			for _, label := range issue.Labels {
				items = append(items, labelGQLObject(repo, label))
			}
			return e.selectConnection(buildGQLConnection(items, args), selections)
		case "comments":
			comments := repo.Comments[issue.Number]
			var items []*GqlObject
			for _, comment := range comments {
				co := newGqlObject("IssueComment")
				co.set("id", JStrVal(globalId("IssueComment", intStr(repo.ID), intStr(comment.ID))))
				co.set("body", JStrVal(comment.Body))
				co.set("createdAt", JStrVal(comment.CreatedAt))
				items = append(items, co)
			}
			return e.selectConnection(buildGQLConnection(items, args), selections)
		case "repository":
			return e.selectObject(repoGQLObject(repo), selections)
		case "author":
			return e.selectObject(userGQLObject(viewerLogin), selections)
		}
		if containsStr([]string{"id", "number", "title", "body", "state", "createdAt", "updatedAt", "url"}, field) {
			if v, ok := object.get(field); ok {
				return v
			}
		}
		e.pushError(fmt.Sprintf("Cannot query field \"%s\" on type \"Issue\".", field))
		return JNullVal()
	}

	if typ == "PullRequest" {
		repo := object.Repo
		pull := object.Pull
		switch field {
		case "headRef":
			refObj := newGqlObject("Ref")
			refObj.set("name", JStrVal(pull.HeadRef))
			target := newGqlObject("GitObject")
			target.set("oid", JStrVal(pull.HeadSha))
			refObj.set("target", JObjRefVal(target))
			return e.selectObject(refObj, selections)
		case "baseRef":
			refObj := newGqlObject("Ref")
			refObj.set("name", JStrVal(pull.BaseRef))
			target := newGqlObject("GitObject")
			target.set("oid", JStrVal(pull.BaseSha))
			refObj.set("target", JObjRefVal(target))
			return e.selectObject(refObj, selections)
		case "mergeable":
			if pullMergeable(repo, pull) {
				return JStrVal("MERGEABLE")
			}
			return JStrVal("CONFLICTING")
		case "files":
			var items []*GqlObject
			for _, f := range pullFiles(repo, pull) {
				fo := newGqlObject("PullRequestFile")
				fo.set("path", JStrVal(f.Filename))
				fo.set("additions", JIntVal(int64(f.Additions)))
				fo.set("deletions", JIntVal(int64(f.Deletions)))
				changeType := "MODIFIED"
				if f.Status == "added" {
					changeType = "ADDED"
				} else if f.Status == "removed" {
					changeType = "REMOVED"
				}
				fo.set("changeType", JStrVal(changeType))
				fo.set("status", JStrVal(f.Status))
				items = append(items, fo)
			}
			return e.selectConnection(buildGQLConnection(items, args), selections)
		case "repository":
			return e.selectObject(repoGQLObject(repo), selections)
		case "author":
			return e.selectObject(userGQLObject(viewerLogin), selections)
		}
		if containsStr([]string{"id", "number", "title", "body", "state", "headRefName", "baseRefName", "merged", "createdAt", "updatedAt", "url"}, field) {
			if v, ok := object.get(field); ok {
				return v
			}
		}
		e.pushError(fmt.Sprintf("Cannot query field \"%s\" on type \"PullRequest\".", field))
		return JNullVal()
	}

	// Any other object type: plain fields from the object itself.
	if v, ok := object.get(field); ok {
		if v.Kind == JArrRef {
			if selections != nil {
				var items []*JVal
				for _, item := range v.ArrRef {
					items = append(items, e.selectObject(item, selections))
				}
				return JArrVal(items...)
			}
			var items []*JVal
			for range v.ArrRef {
				items = append(items, JObjVal())
			}
			return JArrVal(items...)
		}
		if v.Kind == JObjRef {
			if selections != nil {
				return e.selectObject(v.ObjRef, selections)
			}
			return gqlObjectJSON(v.ObjRef)
		}
		return v
	}
	e.pushError(fmt.Sprintf("Cannot query field \"%s\" on type \"%s\".", field, typ))
	return JNullVal()
}

func argOrUndef(args map[string]*JVal, name string) *JVal {
	if v, ok := args[name]; ok {
		return v
	}
	return JUndefVal()
}

// gqlNumberArg mirrors Number(args.number) used as a map key: only integer
// values resolve.
func gqlNumberArg(args map[string]*JVal, name string) (float64, bool) {
	v, ok := args[name]
	if !ok || v == nil {
		return 0, false
	}
	n, numOK := jsNumber(v)
	if !numOK || math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
		return 0, false
	}
	return n, true
}

func gqlStatesArg(args map[string]*JVal, name string) string {
	v, ok := args[name]
	if !ok || v == nil || v.Kind != JArr {
		return "all"
	}
	hasOpen, hasClosed := false, false
	for _, item := range v.Arr {
		if item.Kind == JStr {
			if item.Str == "OPEN" {
				hasOpen = true
			}
			if item.Str == "CLOSED" {
				hasClosed = true
			}
		}
	}
	if hasOpen && hasClosed {
		return "all"
	}
	if hasOpen {
		return "open"
	}
	return "closed"
}

// selectObject executes a selection set against an object ({} when there is
// no selection set).
func (e *gqlExecutor) selectObject(object *GqlObject, selections []*gqlSelection) *JVal {
	out := &JVal{Kind: JObj}
	if selections == nil {
		return out
	}
	e.executeSelections(object, selections, out)
	return out
}

// selectConnection executes a selection set against a connection value.
func (e *gqlExecutor) selectConnection(conn *gqlConnection, selections []*gqlSelection) *JVal {
	if selections == nil {
		out := &JVal{Kind: JObj}
		out.ObjSet("totalCount", JIntVal(int64(conn.TotalCount)))
		var edgeVals []*JVal
		for _, edge := range conn.Edges {
			edgeVals = append(edgeVals, JObjVal(
				JMember{"cursor", JStrVal(edge.Cursor)},
				JMember{"node", gqlObjectJSON(edge.Node)},
			))
		}
		out.ObjSet("edges", JArrVal(edgeVals...))
		var nodeVals []*JVal
		for _, node := range conn.Nodes {
			nodeVals = append(nodeVals, gqlObjectJSON(node))
		}
		out.ObjSet("nodes", JArrVal(nodeVals...))
		out.ObjSet("pageInfo", e.pageInfoJSON(conn))
		return out
	}
	out := &JVal{Kind: JObj}
	for _, sel := range selections {
		if sel.kind != "field" {
			continue
		}
		key := sel.name
		if sel.hasAlias {
			key = sel.alias
		}
		switch sel.name {
		case "edges":
			var edgeVals []*JVal
			for _, edge := range conn.Edges {
				edgeOut := &JVal{Kind: JObj}
				for _, sub := range sel.selections {
					if sub.kind != "field" {
						continue
					}
					subKey := sub.name
					if sub.hasAlias {
						subKey = sub.alias
					}
					if sub.name == "cursor" {
						edgeOut.ObjSet(subKey, JStrVal(edge.Cursor))
					} else if sub.name == "node" {
						edgeOut.ObjSet(subKey, e.selectObject(edge.Node, sub.selections))
					}
				}
				edgeVals = append(edgeVals, edgeOut)
			}
			out.ObjSet(key, JArrVal(edgeVals...))
		case "nodes":
			var nodeVals []*JVal
			for _, node := range conn.Nodes {
				nodeVals = append(nodeVals, e.selectObject(node, sel.selections))
			}
			out.ObjSet(key, JArrVal(nodeVals...))
		case "pageInfo":
			pageOut := &JVal{Kind: JObj}
			for _, sub := range sel.selections {
				if sub.kind != "field" {
					continue
				}
				subKey := sub.name
				if sub.hasAlias {
					subKey = sub.alias
				}
				pageOut.ObjSet(subKey, e.pageInfoField(conn, sub.name))
			}
			out.ObjSet(key, pageOut)
		case "totalCount":
			out.ObjSet(key, JIntVal(int64(conn.TotalCount)))
		default:
			out.ObjSet(key, JNullVal())
			e.pushError(fmt.Sprintf("Cannot query field \"%s\" on type \"Connection\".", sel.name))
		}
	}
	return out
}

func (e *gqlExecutor) pageInfoJSON(conn *gqlConnection) *JVal {
	out := &JVal{Kind: JObj}
	out.ObjSet("hasNextPage", JBoolVal(conn.HasNextPage))
	out.ObjSet("hasPreviousPage", JBoolVal(conn.HasPreviousPage))
	if conn.StartCursor != nil {
		out.ObjSet("startCursor", JStrVal(*conn.StartCursor))
	} else {
		out.ObjSet("startCursor", JNullVal())
	}
	if conn.EndCursor != nil {
		out.ObjSet("endCursor", JStrVal(*conn.EndCursor))
	} else {
		out.ObjSet("endCursor", JNullVal())
	}
	return out
}

func (e *gqlExecutor) pageInfoField(conn *gqlConnection, name string) *JVal {
	switch name {
	case "hasNextPage":
		return JBoolVal(conn.HasNextPage)
	case "hasPreviousPage":
		return JBoolVal(conn.HasPreviousPage)
	case "startCursor":
		if conn.StartCursor != nil {
			return JStrVal(*conn.StartCursor)
		}
		return JNullVal()
	case "endCursor":
		if conn.EndCursor != nil {
			return JStrVal(*conn.EndCursor)
		}
		return JNullVal()
	}
	return JNullVal()
}

func (e *gqlExecutor) resolveMutation(field string, args map[string]*JVal, selections []*gqlSelection) *JVal {
	input := &JVal{Kind: JObj}
	if v, ok := args["input"]; ok && v != nil && v.Kind == JObj {
		input = v
	}
	clientMutationID := JNullVal()
	if v := input.ObjGet("clientMutationId"); v != nil && v.Kind != JUndef && v.Kind != JNull {
		clientMutationID = v
	}

	wrap := func(payload map[string]*JVal) *JVal {
		if selections == nil {
			out := &JVal{Kind: JObj}
			for k, v := range payload {
				out.ObjSet(k, gqlPayloadValue(v))
			}
			return out
		}
		result := &JVal{Kind: JObj}
		for _, sel := range selections {
			if sel.kind != "field" {
				continue
			}
			key := sel.name
			if sel.hasAlias {
				key = sel.alias
			}
			if sel.name == "clientMutationId" {
				result.ObjSet(key, clientMutationID)
				continue
			}
			value, ok := payload[sel.name]
			if !ok || value == nil {
				result.ObjSet(key, JNullVal())
				continue
			}
			if value.Kind == JObjRef {
				result.ObjSet(key, e.selectObject(value.ObjRef, sel.selections))
				continue
			}
			if value.Kind == JObj {
				if node := value.ObjGet("node"); node != nil && node.Kind == JObjRef {
					edgeOut := &JVal{Kind: JObj}
					for _, sub := range sel.selections {
						if sub.kind != "field" {
							continue
						}
						subKey := sub.name
						if sub.hasAlias {
							subKey = sub.alias
						}
						if sub.name == "node" {
							edgeOut.ObjSet(subKey, e.selectObject(node.ObjRef, sub.selections))
						} else if sub.name == "cursor" {
							edgeOut.ObjSet(subKey, JStrVal(cursorFor(0)))
						}
					}
					result.ObjSet(key, edgeOut)
					continue
				}
			}
			if value.Kind == JUndef {
				result.ObjSet(key, JNullVal())
			} else {
				result.ObjSet(key, value)
			}
		}
		return result
	}

	strInput := func(name string) string {
		return jsStringify(inputFieldOrUndef(input, name))
	}

	switch field {
	case "createIssue":
		repoObj := findByGlobalID(e.store, strInput("repositoryId"))
		if repoObj == nil || repoObj.Type != "Repository" {
			e.pushError("createIssue: repository not found")
			return JNullVal()
		}
		repo := repoObj.Repo
		issue, errMsg := createIssue(e.store, repo, strInput("title"), strInput("body"), nil)
		if errMsg != "" {
			e.pushError(errMsg)
			return JNullVal()
		}
		return wrap(map[string]*JVal{"issue": JObjRefVal(issueGQLObject(repo, issue))})
	case "closeIssue":
		found := findByGlobalID(e.store, strInput("issueId"))
		if found == nil || found.Type != "Issue" {
			e.pushError("closeIssue: issue not found")
			return JNullVal()
		}
		repo := found.Repo
		issue := found.Issue
		closed := "closed"
		patched := patchIssue(repo, issue.Number, &closed, nil, nil)
		if patched == nil {
			e.pushError("closeIssue: issue not found")
			return JNullVal()
		}
		return wrap(map[string]*JVal{"issue": JObjRefVal(issueGQLObject(repo, patched))})
	case "createPullRequest":
		repoObj := findByGlobalID(e.store, strInput("repositoryId"))
		if repoObj == nil || repoObj.Type != "Repository" {
			e.pushError("createPullRequest: repository not found")
			return JNullVal()
		}
		repo := repoObj.Repo
		pull, errMsg := createPull(repo, strInput("headRefName"), strInput("baseRefName"), strInput("title"), strInput("body"))
		if errMsg != "" {
			e.pushError(errMsg)
			return JNullVal()
		}
		return wrap(map[string]*JVal{"pullRequest": JObjRefVal(pullGQLObject(repo, pull))})
	case "mergePullRequest":
		found := findByGlobalID(e.store, strInput("pullRequestId"))
		if found == nil || found.Type != "PullRequest" {
			e.pushError("mergePullRequest: pull request not found")
			return JNullVal()
		}
		repo := found.Repo
		pull := found.Pull
		_, errMsg, _ := mergePull(repo, pull.Number)
		if errMsg != "" {
			e.pushError(errMsg)
			return JNullVal()
		}
		updated := repo.Pulls[pull.Number]
		return wrap(map[string]*JVal{"pullRequest": JObjRefVal(pullGQLObject(repo, updated))})
	case "addComment":
		found := findByGlobalID(e.store, strInput("subjectId"))
		if found == nil || found.Type != "Issue" {
			e.pushError("addComment: subject not found")
			return JNullVal()
		}
		repo := found.Repo
		issue := found.Issue
		comment := createComment(repo, issue.Number, strInput("body"))
		if comment == nil {
			e.pushError("addComment: subject not found")
			return JNullVal()
		}
		node := newGqlObject("IssueComment")
		node.set("id", JStrVal(globalId("IssueComment", intStr(repo.ID), intStr(comment.ID))))
		node.set("body", JStrVal(comment.Body))
		node.set("createdAt", JStrVal(comment.CreatedAt))
		edge := &JVal{Kind: JObj}
		edge.ObjSet("node", JObjRefVal(node))
		return wrap(map[string]*JVal{"commentEdge": edge})
	}
	e.pushError(fmt.Sprintf("Cannot query field \"%s\" on type \"Mutation\".", field))
	return JNullVal()
}

// gqlPayloadValue converts a mutation payload value for the no-selections
// path (raw object rendering).
func gqlPayloadValue(v *JVal) *JVal {
	if v == nil {
		return JNullVal()
	}
	if v.Kind == JObjRef {
		return gqlObjectJSON(v.ObjRef)
	}
	if v.Kind == JObj {
		out := &JVal{Kind: JObj}
		for _, m := range v.Obj {
			out.ObjSet(m.Key, gqlPayloadValue(m.Val))
		}
		return out
	}
	return v
}

func inputFieldOrUndef(input *JVal, name string) *JVal {
	if v := input.ObjGet(name); v != nil {
		return v
	}
	return JUndefVal()
}

// --- HTTP entry point ---

func graphqlAuthFail() HttpResult {
	return HttpResult{Status: 401, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: []byte(`{"message":"Requires authentication"}`)}
}

func handleGraphql(store *Store, headers map[string]string, body []byte) HttpResult {
	if ok, _ := checkAuth(headers); !ok {
		return graphqlAuthFail()
	}
	payload, err := ParseJSON(body)
	if err != nil {
		return HttpResult{Status: 400, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: []byte(`{"errors":[{"message":"Problems parsing JSON"}]}`)}
	}
	var requests []*JVal
	isArray := payload.Kind == JArr
	if isArray {
		requests = payload.Arr
	} else {
		requests = []*JVal{payload}
	}
	results := make([]*JVal, 0, len(requests))
	for _, req := range requests {
		results = append(results, executeGQLOne(store, req))
	}
	var out *JVal
	if isArray {
		out = JArrVal(results...)
	} else {
		out = results[0]
	}
	return HttpResult{Status: 200, Headers: map[string]string{"Content-Type": "application/json; charset=utf-8"}, Body: out.Marshal()}
}

func executeGQLOne(store *Store, request *JVal) *JVal {
	// A null request throws in the reference (reading .query of null); let
	// that surface as a 500 via the dispatcher's panic recovery.
	if request == nil || request.Kind == JNull {
		panic("cannot read query of null")
	}
	query := ""
	if request.Kind == JObj {
		if v := request.ObjGet("query"); v != nil && v.Kind == JStr {
			query = v.Str
		}
	}
	variables := map[string]*JVal{}
	if request.Kind == JObj {
		if v := request.ObjGet("variables"); v != nil && v.Kind == JObj {
			for _, m := range v.Obj {
				variables[m.Key] = m.Val
			}
		}
	}
	var operationName *string
	if request.Kind == JObj {
		if v := request.ObjGet("operationName"); v != nil && v.Kind == JStr {
			s := v.Str
			operationName = &s
		}
	}
	if query == "" {
		return JObjVal(JMember{"errors", JArrVal(JObjVal(JMember{"message", JStrVal("Must provide query string.")}))})
	}
	tokens, err := tokenizeGQL(query)
	if err != nil {
		return gqlErrorResult(err.Error())
	}
	parser := &gqlParser{tokens: tokens}
	operations, fragments, err := parser.parseDocument()
	if err != nil {
		return gqlErrorResult(err.Error())
	}
	var operation *gqlOperation
	if operationName == nil {
		if len(operations) > 0 {
			operation = operations[0]
		}
	} else {
		for _, op := range operations {
			if op.hasName && op.name == *operationName {
				operation = op
				break
			}
		}
	}
	if operation == nil {
		name := "undefined"
		if operationName != nil {
			name = *operationName
		}
		return gqlErrorResult(fmt.Sprintf("Unknown operation named \"%s\".", name))
	}
	exec := &gqlExecutor{store: store, variables: variables, fragments: fragments}
	rootType := "Query"
	if operation.kind == "mutation" {
		rootType = "Mutation"
	}
	root := newGqlObject(rootType)
	data := &JVal{Kind: JObj}
	exec.executeSelections(root, operation.selections, data)
	result := &JVal{Kind: JObj}
	result.ObjSet("data", data)
	if len(exec.errors) > 0 {
		result.ObjSet("errors", JArrVal(exec.errors...))
	}
	return result
}

func gqlErrorResult(msg string) *JVal {
	return JObjVal(JMember{"errors", JArrVal(JObjVal(JMember{"message", JStrVal(msg)}))})
}
