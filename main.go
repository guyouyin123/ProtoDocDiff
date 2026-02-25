package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	ApiPath     string
	Envs        []string
	RootDirMap  map[string]string
	DocDir      string // 文档生成路径
	MaxBranches int    //进一月活跃分支数量
	ConsulAddr  string // consul地址
)

func init() {
	loadConfYAML()
	if DocDir == "" || len(RootDirMap) == 0 {
		panic("conf.yaml missing required fields: docDir/rootDirMap")
	}
}

func main() {
	docRun()
}

// Minimal YAML loader for current config schema without external deps
func loadConfYAML() {
	cwd, _ := os.Getwd()
	// try current and two parent directories
	tryDirs := []string{cwd, filepath.Dir(cwd), filepath.Dir(filepath.Dir(cwd))}
	var data []byte
	var err error
	for _, d := range tryDirs {
		p := filepath.Join(d, "conf.yaml")
		if b, e := os.ReadFile(p); e == nil {
			data = b
			err = nil
			break
		} else {
			err = e
		}
	}
	if err != nil || len(data) == 0 {
		return
	}
	var cfg struct {
		ApiPath     string            `yaml:"ApiPath"`
		Envs        []string          `yaml:"Envs"`
		RootDirMap  map[string]string `yaml:"RootDirMap"`
		DocDir      string            `yaml:"DocDir"`
		MaxBranches int               `yaml:"MaxBranches"`
		ConsulAddr  string            `yaml:"ConsulAddr"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return
	}
	ApiPath = strings.TrimSpace(cfg.ApiPath)
	DocDir = strings.TrimSpace(cfg.DocDir)
	ConsulAddr = strings.TrimSpace(cfg.ConsulAddr)
	MaxBranches = cfg.MaxBranches
	Envs = append([]string{}, cfg.Envs...)
	RootDirMap = map[string]string{}
	for k, v := range cfg.RootDirMap {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			RootDirMap[k] = v
		}
	}
}

func splitTopKV(line string) (string, string, bool) {
	i := strings.Index(line, ":")
	if i <= 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:i])
	v := strings.TrimSpace(line[i+1:])
	v = stripInlineComment(v)
	v = trimQuotes(v)
	return k, v, true
}

func splitKeyVal(line string) (string, string, bool) {
	i := strings.Index(line, ":")
	if i <= 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:i])
	k = trimQuotes(k)
	v := strings.TrimSpace(line[i+1:])
	v = stripInlineComment(v)
	v = trimQuotes(v)
	if k == "" || v == "" {
		return "", "", false
	}
	return k, v, true
}

func trimQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func stripInlineComment(s string) string {
	inSingle := false
	inDouble := false
	for i, r := range s {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return strings.TrimSpace(s)
}

func docRun() {
	if wd, _ := os.Getwd(); wd != "" {
		_ = os.MkdirAll(DocDir, 0o755)
		mdSrc := filepath.Join(wd, "README.md")
		htmlSrc := filepath.Join(wd, "readme.html")
		if b, err := os.ReadFile(mdSrc); err == nil {
			_ = os.WriteFile(filepath.Join(DocDir, "README.md"), b, 0o644)
		} else {
			fmt.Println("[WARN] 读取 doc/README.md 失败：", err)
		}
		if b, err := os.ReadFile(htmlSrc); err == nil {
			_ = os.WriteFile(filepath.Join(DocDir, "readme.html"), b, 0o644)
		} else {
			fmt.Println("[WARN] 读取 doc/readme.html 失败：", err)
		}
	}
	var rootItems [][2]string
	type projectItem struct{ Name, Link, Latest string }
	for name, root := range RootDirMap {
		groupKey := deriveGroupKey(root)
		groupDir := filepath.Join(DocDir, groupKey)
		_ = os.MkdirAll(groupDir, 0o755)
		var items []projectItem
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			// Skip hidden dirs or common non-project dirs at the top level
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if !e.IsDir() {
				continue
			}
			if e.Name() == filepath.Base(groupDir) {
				continue
			}
			projPath := filepath.Join(root, e.Name())
			projectName := e.Name()

			// Pre-check: Must have 'api' dir and at least one .proto file
			apiDir := filepath.Join(projPath, "api")
			// Use os.ReadDir for apiDir entries, but need to declare variable name distinct from outer loop
			if info, err := os.Stat(apiDir); err != nil || !info.IsDir() {
				// No api dir, skip
				continue
			}
			hasProto := false
			apiEntries, _ := os.ReadDir(apiDir)
			for _, f := range apiEntries {
				if !f.IsDir() && strings.HasSuffix(f.Name(), ".proto") {
					hasProto = true
					break
				}
			}
			if !hasProto {
				// No proto files, skip
				continue
			}

			// Get branches
			branches, err := getDocBranches(projPath)
			if err != nil {
				branches = []BranchInfo{{Name: "master", Date: ""}}
			}

			projBaseDir := filepath.Join(groupDir, projectName)
			_ = os.MkdirAll(projBaseDir, 0o755)

			// Smart Incremental Build Logic
			// 1. Load previous branches.json
			prevBranchesFile := filepath.Join(projBaseDir, "branches.json")
			prevMap := make(map[string]string) // name -> date
			if pb, err := os.ReadFile(prevBranchesFile); err == nil {
				var oldBranches []BranchInfo
				if json.Unmarshal(pb, &oldBranches) == nil {
					for _, b := range oldBranches {
						prevMap[b.Name] = b.Date
					}
				}
			}

			bj, _ := json.Marshal(branches)
			_ = os.WriteFile(prevBranchesFile, bj, 0o644)

			// Prepare list of valid branch dirs to keep
			keepDirs := make(map[string]bool)
			for _, b := range branches {
				keepDirs[b.Name] = true
			}

			// Iterate branches
			for _, branchInfo := range branches {
				// Check if we can skip generation
				if lastDate, ok := prevMap[branchInfo.Name]; ok {
					// If date matches and directory exists, skip
					// Note: branch name with slashes corresponds to nested dirs, but we check leaf dir existence
					// Ideally we check index.html existence
					targetHtml := filepath.Join(groupDir, projectName, branchInfo.Name, "index.html")
					if lastDate == branchInfo.Date {
						if _, err := os.Stat(targetHtml); err == nil {
							// Up to date, skip
							fmt.Printf("[SKIP] %s | %s (Up to date)\n", projectName, branchInfo.Name)
							continue
						}
					}
				}

				fmt.Printf("[GEN]  %s | %s\n", projectName, branchInfo.Name)

				func() {
					branch := branchInfo.Name
					defer func() { recover() }()

					// Setup worktree
					wtDir, cleanup, err := setupWorktree(projPath, branch)
					if err != nil {
						// Quietly ignore checkout failures (e.g. invalid ref)
						// fmt.Println("[WARN] Failed to checkout", branch, "for", projectName, err)
						return
					}
					defer cleanup()

					out := buildProject(wtDir, projectName)
					if out == nil {
						return
					}

					// Fix ApiDir to be human readable relative path instead of tmp dir
					// We can extract the last two parts of root + projectName + "/api"
					parts := strings.Split(filepath.Clean(root), string(filepath.Separator))
					var pathBase string
					if len(parts) >= 2 {
						pathBase = strings.Join(parts[len(parts)-2:], "/")
					} else {
						pathBase = filepath.Base(root)
					}
					out.ApiDir = pathBase + "/" + projectName + ApiPath

					if out.Proto != "" {
						out.Proto = pathBase + "/" + projectName + "/" + out.Proto
					}

					// Compute diff against master/main if available
					if branch != "master" && branch != "main" {
						masterPath := filepath.Join(groupDir, projectName, "master", projectName+"_doc.json")
						if b, err := os.ReadFile(masterPath); err == nil {
							var mo ProjectDoc
							if json.Unmarshal(b, &mo) == nil {
								mm := map[string]MethodDoc{}
								for _, m := range mo.Methods {
									mm[m.Name] = m
								}
								for i := range out.Methods {
									cur := &out.Methods[i]
									m, ok := mm[cur.Name]
									if !ok {
										cur.Diff = "NEW"
										continue
									}
									// Request fields
									mf := map[string]Field{}
									for _, f := range m.RequestFld {
										mf[f.Name] = f
									}
									var newReq, modReq []string
									for _, f := range cur.RequestFld {
										if of, exists := mf[f.Name]; !exists {
											newReq = append(newReq, f.Name)
										} else if strings.TrimSpace(of.Type) != strings.TrimSpace(f.Type) || strings.TrimSpace(of.Comment) != strings.TrimSpace(f.Comment) {
											modReq = append(modReq, f.Name)
										}
									}
									// Response fields
									mr := map[string]string{}
									for _, rf := range m.ResponseFld {
										mr[rf["name"]] = strings.TrimSpace(rf["comment"])
									}
									var newResp, modResp []string
									for _, rf := range cur.ResponseFld {
										nm := rf["name"]
										cm := strings.TrimSpace(rf["comment"])
										if ov, exists := mr[nm]; !exists {
											newResp = append(newResp, nm)
										} else if ov != cm {
											modResp = append(modResp, nm)
										}
									}
									cur.NewFields = newReq
									cur.ModifiedFlds = modReq
									cur.NewRespFlds = newResp
									cur.ModifiedResp = modResp
									if len(newReq)+len(modReq)+len(newResp)+len(modResp) > 0 && cur.Diff == "" {
										cur.Diff = "MODIFIED"
									}
								}
							}
						}
					}

					_, _, err = writeProjectFiles(DocDir, groupKey, projectName, branch, *out)
					if err != nil {
						fmt.Println("[ERROR]", projectName, branch, err)
						return
					}
				}()
			}
			filepath.WalkDir(projBaseDir, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if !d.IsDir() {
					return nil
				}
				// Check if this dir is a branch root (has index.html and doc.json)
				// Construct branch name from path relative to projBaseDir
				rel, _ := filepath.Rel(projBaseDir, path)
				if rel == "." {
					return nil
				}

				// If this directory contains index.html, assume it's a generated doc root
				if _, err := os.Stat(filepath.Join(path, "index.html")); err == nil {
					if !keepDirs[rel] {
						// It's stale!
						os.RemoveAll(path)
						return filepath.SkipDir // No need to go deeper
					}
				}
				return nil
			})
			defaultBranch := "master"
			if len(branches) > 0 {
				defaultBranch = branches[0].Name
			}
			link := projectName + "/" + defaultBranch + "/index.html"
			// compute latest commit time among recent active branches
			latest := ""
			var latestT time.Time
			for _, b := range branches {
				if strings.TrimSpace(b.Date) == "" {
					continue
				}
				if t, err := time.Parse("2006-01-02 15:04:05", b.Date); err == nil {
					if t.After(latestT) {
						latestT = t
						latest = b.Date
					}
				}
			}
			items = append(items, projectItem{Name: projectName, Link: link, Latest: latest})
		}
		sort.Slice(items, func(i, j int) bool { return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name) })
		var sb strings.Builder
		for _, it := range items {
			if it.Latest != "" {
				sb.WriteString(fmt.Sprintf("<li data-latest='%s'><a href='%s'>%s</a></li>\n", it.Latest, it.Link, it.Name))
			} else {
				sb.WriteString(fmt.Sprintf("<li><a href='%s'>%s</a></li>\n", it.Link, it.Name))
			}
		}
		itemsHTML := sb.String()
		if itemsHTML == "" {
			itemsHTML = "<li>无项目可生成</li>"
		}
		brand := name + " API文档导航"
		nav := strings.ReplaceAll(indexTemplate, "{items}", itemsHTML)
		nav = strings.ReplaceAll(nav, "{brand}", brand)
		nav = strings.ReplaceAll(nav, "{readme_link}", "../../readme.html")
		nav = strings.ReplaceAll(nav, "{homebtn}", "<a class=\"homebtn\" href=\"../../index.html\">返回上一级</a>")
		// Inject a small script to replace sub text with latest commit time for this group page only
		nav = strings.Replace(nav, "</body>", "<script>(function(){var list=document.getElementById('list'); if(!list) return; Array.from(list.children).forEach(function(li){ var latest=li.getAttribute('data-latest'); if(latest){ var sub=li.querySelector('.sub'); if(sub){ sub.textContent='最近提交：'+latest; } } });})();</script></body>", 1)
		os.WriteFile(filepath.Join(groupDir, "index.html"), []byte(nav), 0o644)
		fmt.Printf("Generated %d project docs under %s\n", len(items), groupDir)
		rootItems = append(rootItems, [2]string{name + " API文档导航", filepath.Join(groupKey, "index.html")})
	}

	sort.Slice(rootItems, func(i, j int) bool { return strings.ToLower(rootItems[i][0]) < strings.ToLower(rootItems[j][0]) })
	var rb strings.Builder
	for _, it := range rootItems {
		rb.WriteString(fmt.Sprintf("<li><a href='%s'>%s</a></li>\n", it[1], it[0]))
	}
	rItemsHTML := rb.String()
	if rItemsHTML == "" {
		rItemsHTML = "<li>无项目可生成</li>"
	}
	rnav := strings.ReplaceAll(indexTemplate, "{items}", rItemsHTML)
	rnav = strings.ReplaceAll(rnav, "{brand}", "API 文档导航")
	rnav = strings.ReplaceAll(rnav, "{readme_link}", "readme.html")
	rnav = strings.ReplaceAll(rnav, "{homebtn}", "")
	os.WriteFile(filepath.Join(DocDir, "index.html"), []byte(rnav), 0o644)
}

type Field struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Comment  string `json:"comment"`
	Required bool   `json:"required"`
	File     string `json:"-"`
}

type StructInfo struct {
	Fields []Field
	File   string
}

type MethodDoc struct {
	Seq          int                    `json:"seq"`
	Name         string                 `json:"name"`
	Comment      string                 `json:"comment"`
	Display      string                 `json:"display_name"`
	GRPCPath     string                 `json:"grpc_path"`
	Handler      string                 `json:"handler_file"`
	Method       string                 `json:"method"`
	Category     string                 `json:"category"`
	ReqType      string                 `json:"req_type"`
	RespType     string                 `json:"resp_type"`
	GitCreatedBy string                 `json:"git_created_by"`
	GitLastModBy string                 `json:"git_last_modified"`
	GitCreatedAt string                 `json:"git_created_at"`
	GitLastModAt string                 `json:"git_last_modified_at"`
	Request      map[string]interface{} `json:"request"`
	RequestFld   []Field                `json:"request_fields"`
	ResponseFld  []map[string]string    `json:"response_fields"`
	RespEnv      map[string]interface{} `json:"response_envelope"`
	Diff         string                 `json:"diff"`
	NewFields    []string               `json:"new_fields"`
	ModifiedFlds []string               `json:"modified_fields"`
	NewRespFlds  []string               `json:"new_resp_fields"`
	ModifiedResp []string               `json:"modified_resp_fields"`
	ProtoLine    int                    `json:"-"`
}

type ProjectDoc struct {
	Service string                 `json:"service"`
	Proto   string                 `json:"proto"`
	ApiDir  string                 `json:"api_dir"`
	Methods []MethodDoc            `json:"methods"`
	Stats   map[string]interface{} `json:"stats"`
}

var builtinDefaults = map[string]interface{}{
	"int": 0, "int8": 0, "int16": 0, "int32": 0, "int64": 0,
	"uint": 0, "uint8": 0, "uint16": 0, "uint32": 0, "uint64": 0,
	"float32": 0.0, "float64": 0.0,
	"string": "", "bool": false,
}

func readFile(p string) string { b, _ := os.ReadFile(p); return string(b) }

func listGoFiles(root string) []string {
	var out []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func baseType(t string) string {
	s := strings.TrimSpace(t)
	for strings.HasPrefix(s, "*") {
		s = s[1:]
	}
	for strings.HasPrefix(s, "[]") {
		s = s[2:]
	}
	return s
}

func parseStructs(goSources map[string]string) (map[string]StructInfo, map[string]string) {
	structs := make(map[string]StructInfo)
	aliases := make(map[string]string)
	typeStart := regexp.MustCompile(`type\s+(\w+)\s+struct\s*\{`)
	fieldRe := regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s+([^` + "`" + `\s]+)(?:\s*` + "`" + `[^` + "`" + `]*` + "`" + `)?(?:\s*//\s*(.*))?`)
	aliasEq := regexp.MustCompile(`type\s+(\w+)\s*=\s*([^\n]+)`)
	aliasTy := regexp.MustCompile(`type\s+(\w+)\s+(\[\][^\s]+|\*?[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)?)`)
	for path, src := range goSources {
		for _, loc := range typeStart.FindAllStringIndex(src, -1) {
			m := typeStart.FindStringSubmatch(src[loc[0]:loc[1]])
			name := ""
			if len(m) >= 2 {
				name = m[1]
			}
			i := loc[1]
			depth := 1
			for i < len(src) && depth > 0 {
				if src[i] == '{' {
					depth++
				} else if src[i] == '}' {
					depth--
				}
				i++
			}
			block := src[loc[1] : i-1]
			var fields []Field
			scanner := bufio.NewScanner(strings.NewReader(block))
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "//") {
					continue
				}
				fm := fieldRe.FindStringSubmatch(line)
				if len(fm) >= 3 {
					fields = append(fields, Field{Name: fm[1], Type: fm[2], Comment: strings.TrimSpace(fm[3]), File: path})
				}
			}
			structs[name] = StructInfo{Fields: fields, File: path}
		}
		for _, m := range aliasEq.FindAllStringSubmatch(src, -1) {
			aliases[m[1]] = strings.TrimSpace(m[2])
		}
		for _, m := range aliasTy.FindAllStringSubmatch(src, -1) {
			if strings.HasPrefix(strings.ToLower(m[2]), "struct") {
				continue
			}
			aliases[m[1]] = strings.TrimSpace(m[2])
		}
	}
	return structs, aliases
}

func exampleForType(t string, structs map[string]StructInfo, aliases map[string]string, seen map[string]bool) interface{} {
	tt := strings.TrimSpace(t)
	for {
		if v, ok := aliases[tt]; ok {
			tt = strings.TrimSpace(v)
			continue
		}
		break
	}
	if strings.HasPrefix(tt, "[]") {
		inner := strings.TrimSpace(tt[2:])
		return []interface{}{exampleForType(inner, structs, aliases, seen)}
	}
	if strings.HasPrefix(tt, "*") {
		return exampleForType(tt[1:], structs, aliases, seen)
	}
	bt := baseType(tt)
	if _, ok := builtinDefaults[bt]; ok {
		return builtinDefaults[bt]
	}
	if v, ok := aliases[bt]; ok {
		return exampleForType(v, structs, aliases, seen)
	}
	if si, ok := structs[bt]; ok && !seen[bt] {
		seen[bt] = true
		obj := make(map[string]interface{})
		for _, f := range si.Fields {
			obj[f.Name] = exampleForType(f.Type, structs, aliases, seen)
		}
		delete(seen, bt)
		return obj
	}
	return map[string]interface{}{}
}

func buildSchemaForFields(fields []Field, structs map[string]StructInfo, aliases map[string]string) map[string]interface{} {
	obj := make(map[string]interface{})
	for _, f := range fields {
		obj[f.Name] = exampleForType(f.Type, structs, aliases, map[string]bool{})
	}
	return obj
}

func buildFieldListForFields(fields []Field) []map[string]string {
	var out []map[string]string
	for _, f := range fields {
		out = append(out, map[string]string{"name": f.Name, "comment": f.Comment})
	}
	return out
}

func buildSchema(structName string, structs map[string]StructInfo, aliases map[string]string) map[string]interface{} {
	if structName == "" {
		return map[string]interface{}{}
	}
	resolved := structName
	if _, ok := structs[resolved]; !ok {
		if v, ok := aliases[resolved]; ok {
			for {
				if vv, ok2 := aliases[v]; ok2 {
					v = vv
				} else {
					break
				}
			}
			if _, ok := structs[v]; ok {
				resolved = v
			} else {
				return map[string]interface{}{}
			}
		} else {
			return map[string]interface{}{}
		}
	}
	obj := make(map[string]interface{})
	for _, f := range structs[resolved].Fields {
		obj[f.Name] = exampleForType(f.Type, structs, aliases, map[string]bool{})
	}
	return obj
}

func buildFieldList(structName string, structs map[string]StructInfo, aliases map[string]string) []Field {
	if structName == "" {
		return []Field{}
	}
	if _, ok := structs[structName]; !ok {
		return []Field{}
	}
	var out []Field
	for _, f := range structs[structName].Fields {
		req := false
		if f.Comment != "" && (strings.Contains(f.Comment, "必填") || strings.Contains(f.Comment, "必选") || strings.Contains(f.Comment, "必须")) {
			req = true
		}
		out = append(out, Field{Name: f.Name, Type: f.Type, Comment: f.Comment, Required: req})
	}
	return out
}

func flattenResponseFields(structName string, structs map[string]StructInfo, aliases map[string]string, prefix string, seen map[string]bool) []map[string]string {
	if structName == "" {
		return []map[string]string{}
	}
	if _, ok := structs[structName]; !ok {
		return []map[string]string{}
	}
	if seen == nil {
		seen = map[string]bool{}
	}
	if seen[structName] {
		return []map[string]string{}
	}
	seen[structName] = true
	var out []map[string]string
	for _, f := range structs[structName].Fields {
		name := prefix + f.Name
		out = append(out, map[string]string{"name": name, "comment": f.Comment})
		inner := strings.TrimSpace(f.Type)
		for strings.HasPrefix(inner, "[]") {
			inner = inner[2:]
		}
		if strings.HasPrefix(inner, "*") {
			inner = inner[1:]
		}
		resolved := inner
		for {
			if v, ok := aliases[resolved]; ok {
				resolved = v
			} else {
				break
			}
		}
		for strings.HasPrefix(resolved, "[]") {
			resolved = resolved[2:]
		}
		if strings.HasPrefix(resolved, "*") {
			resolved = resolved[1:]
		}
		if _, ok := structs[resolved]; ok {
			nested := flattenResponseFields(resolved, structs, aliases, name+".", seen)
			out = append(out, nested...)
		}
	}
	delete(seen, structName)
	return out
}

func resolveFuncReturnType(goSources map[string]string, funcName string) string {
	patMulti := regexp.MustCompile(`func\s+` + regexp.QuoteMeta(funcName) + `\s*\([^)]*\)\s*\(([^)]*)\)`)
	patSingle := regexp.MustCompile(`func\s+` + regexp.QuoteMeta(funcName) + `\s*\([^)]*\)\s*([^\s\{]+)\s*\{`)
	choose := func(ret string) string {
		parts := []string{}
		for _, p := range strings.Split(ret, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				parts = append(parts, p)
			}
		}
		for _, p := range parts {
			bp := baseType(p)
			if strings.ToLower(bp) == "error" {
				continue
			}
			ss := strings.Split(bp, ".")
			return ss[len(ss)-1]
		}
		return ""
	}
	for _, src := range goSources {
		if m := patMulti.FindStringSubmatch(src); len(m) > 1 {
			if rt := choose(m[1]); rt != "" {
				return rt
			}
		}
		if m := patSingle.FindStringSubmatch(src); len(m) > 1 {
			if rt := choose(m[1]); rt != "" {
				return rt
			}
		}
	}
	return ""
}

func findHandlerReqType(methodName string, goSources map[string]string) (string, string) {
	header := regexp.MustCompile(`func\s*\(\s*\w+\s*\*\s*(?:[sS]erver)\s*\)\s*` + regexp.QuoteMeta(methodName) + `\s*\([^)]*\)\s*(?:\([^)]*\)\s*)?\{`)
	nextFn := regexp.MustCompile(`\nfunc\s*\(`)
	builtin := func(t string) bool { _, ok := builtinDefaults[strings.ToLower(baseType(t))]; return ok }
	for path, src := range goSources {
		loc := header.FindStringIndex(src)
		if loc == nil {
			continue
		}
		start := loc[1]
		m := nextFn.FindStringIndex(src[start:])
		end := len(src)
		if m != nil {
			end = start + m[0]
		}
		body := src[start:end]
		um := regexp.MustCompile(`json\.Unmarshal\(\s*[^,]+,\s*&?\s*(\w+)\s*\)`).FindStringSubmatch(body)
		varVar := ""
		if len(um) > 1 {
			varVar = um[1]
		}
		if varVar != "" {
			if mm := regexp.MustCompile(`var\s+` + regexp.QuoteMeta(varVar) + `\s+([A-Za-z0-9_.]+)`).FindStringSubmatch(body); len(mm) > 1 && !builtin(mm[1]) {
				t := strings.Split(mm[1], ".")[len(strings.Split(mm[1], "."))-1]
				return t, path
			}
			if mm := regexp.MustCompile(`\b` + regexp.QuoteMeta(varVar) + `\s*:\s*=\s*(?:new\(\s*([A-Za-z0-9_.]+)\s*\)|&?\s*([A-Za-z0-9_.]+)\s*\{\})`).FindStringSubmatch(body); len(mm) > 0 {
				tt := mm[1]
				if tt == "" {
					tt = mm[2]
				}
				if !builtin(tt) {
					t := strings.Split(tt, ".")[len(strings.Split(tt, "."))-1]
					return t, path
				}
			}
		}
		if mm := regexp.MustCompile(`var\s+\w+\s+([A-Za-z0-9_.]+)`).FindStringSubmatch(body); len(mm) > 1 && !builtin(mm[1]) {
			t := strings.Split(mm[1], ".")[len(strings.Split(mm[1], "."))-1]
			return t, path
		}
		if mm := regexp.MustCompile(`\w+\s*:\s*=\s*(?:new\(\s*([A-Za-z0-9_.]+)\s*\)|&?\s*([A-Za-z0-9_.]+)\s*\{\})`).FindStringSubmatch(body); len(mm) > 0 {
			tt := mm[1]
			if tt == "" {
				tt = mm[2]
			}
			if !builtin(tt) {
				t := strings.Split(tt, ".")[len(strings.Split(tt, "."))-1]
				return t, path
			}
		}
	}
	return "", ""
}

func findResponseInfo(methodName string, goSources map[string]string, structs map[string]StructInfo, aliases map[string]string) (string, map[string]interface{}, []map[string]string, string) {
	wrappers := []string{`wcode\.NewCommonRet\(\s*([^,]+)\s*,`, `GenerateJSON\(\s*[^,]+\s*,\s*([^\)]+)\)`, `MakeBaseRsp\(\s*[^,]+\s*,\s*[^,]+\s*,\s*([^\)]+)\)`}
	header := regexp.MustCompile(`func\s*\(\s*\w+\s*\*\s*(?:[sS]erver)\s*\)\s*` + regexp.QuoteMeta(methodName) + `\s*\([^)]*\)\s*(?:\([^)]*\)\s*)?\{`)
	nextFn := regexp.MustCompile(`\nfunc\s*\(`)
	for path, src := range goSources {
		loc := header.FindStringIndex(src)
		if loc == nil {
			continue
		}
		start := loc[1]
		m := nextFn.FindStringIndex(src[start:])
		end := len(src)
		if m != nil {
			end = start + m[0]
		}
		body := src[start:end]
		last := ""
		for _, pat := range wrappers {
			re := regexp.MustCompile(pat)
			mm := re.FindAllStringSubmatch(body, -1)
			for _, s := range mm {
				last = strings.TrimSpace(s[1])
			}
		}
		if last != "" {
			varName := strings.TrimLeft(last, "&*")
			if fields := parseAnonymousStruct(body, varName); fields != nil {
				schema := buildSchemaForFields(fields, structs, aliases)
				var fld []map[string]string
				for _, f := range fields {
					fld = append(fld, map[string]string{"name": f.Name, "comment": f.Comment})
				}
				return "", schema, fld, path
			}
			if m := regexp.MustCompile(`var\s+` + regexp.QuoteMeta(varName) + `\s+([A-Za-z0-9_.]+)`).FindStringSubmatch(body); len(m) > 1 {
				t := strings.Split(m[1], ".")[len(strings.Split(m[1], "."))-1]
				return t, nil, nil, path
			}
			if m := regexp.MustCompile(`\b` + regexp.QuoteMeta(varName) + `\s*:\s*=\s*(?:new\(\s*([A-Za-z0-9_.]+)\s*\)|&?\s*([A-Za-z0-9_.]+)\s*\{\})`).FindStringSubmatch(body); len(m) > 0 {
				tt := m[1]
				if tt == "" {
					tt = m[2]
				}
				t := strings.Split(tt, ".")[len(strings.Split(tt, "."))-1]
				return t, nil, nil, path
			}
			if m := regexp.MustCompile(`\b` + regexp.QuoteMeta(varName) + `(?:\s*,\s*\w+)?\s*[:=]=\s*([A-Za-z0-9_.]+)\s*\(`).FindStringSubmatch(body); len(m) > 1 {
				fn := strings.Split(m[1], ".")[len(strings.Split(m[1], "."))-1]
				rt := resolveFuncReturnType(goSources, fn)
				if rt != "" {
					return rt, nil, nil, path
				}
			}
			if m := regexp.MustCompile(`\b` + regexp.QuoteMeta(varName) + `\s*=\s*([A-Za-z0-9_.]+)\s*\(`).FindStringSubmatch(body); len(m) > 1 {
				fn := strings.Split(m[1], ".")[len(strings.Split(m[1], "."))-1]
				rt := resolveFuncReturnType(goSources, fn)
				if rt != "" {
					return rt, nil, nil, path
				}
			}
		}
	}
	return "", nil, nil, ""
}

func parseAnonymousStruct(body string, varName string) []Field {
	re1 := regexp.MustCompile(`var\s+` + regexp.QuoteMeta(varName) + `\s+struct\s*\{([\s\S]*?)\}`)
	re2 := regexp.MustCompile(`\b` + regexp.QuoteMeta(varName) + `\s*:=\s*struct\s*\{([\s\S]*?)\}\s*\{`)
	m := re1.FindStringSubmatch(body)
	if len(m) == 0 {
		m = re2.FindStringSubmatch(body)
	}
	if len(m) == 0 {
		return nil
	}
	block := m[1]
	fieldRe := regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s+([^` + "`" + `\s]+)(?:\s*` + "`" + `[^` + "`" + `]*` + "`" + `)?(?:\s*//\s*(.*))?`)
	var fields []Field
	scanner := bufio.NewScanner(strings.NewReader(block))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		fm := fieldRe.FindStringSubmatch(line)
		if len(fm) >= 3 {
			fields = append(fields, Field{Name: fm[1], Type: fm[2], Comment: strings.TrimSpace(fm[3])})
		}
	}
	return fields
}

func runGit(repoDir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoDir
	var out bytes.Buffer
	var errB bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errB
	if e := cmd.Run(); e != nil {
		return "", fmt.Errorf("%v: %s", e, strings.TrimSpace(errB.String()))
	}
	return out.String(), nil
}

func gitLastAuthor(repoDir, file string, line int) string {
	if line <= 0 {
		return ""
	}
	s, err := runGit(repoDir, "blame", "-w", "--line-porcelain", "-L", fmt.Sprintf("%d,%d", line, line), file)
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(ln, "author ") {
			return strings.TrimSpace(strings.TrimPrefix(ln, "author "))
		}
	}
	return ""
}

func gitLastTime(repoDir, file string, line int) string {
	if line <= 0 {
		return ""
	}
	s, err := runGit(repoDir, "blame", "-w", "--line-porcelain", "-L", fmt.Sprintf("%d,%d", line, line), file)
	if err != nil {
		return ""
	}
	var ts string
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(ln, "author-time ") {
			ts = strings.TrimSpace(strings.TrimPrefix(ln, "author-time "))
			break
		}
	}
	if ts == "" {
		return ""
	}
	if sec, err := strconv.ParseInt(ts, 10, 64); err == nil {
		loc, e := time.LoadLocation("Asia/Shanghai")
		if e != nil {
			loc = time.FixedZone("CST", 8*3600)
		}
		return time.Unix(sec, 0).In(loc).Format("2006-01-02 15:04:05")
	}
	return ts
}

func gitFirstAuthor(repoDir, file string, line int) string {
	if line <= 0 {
		return ""
	}
	s, err := runGit(repoDir, "log", "-L", fmt.Sprintf("%d,%d:%s", line, line, file), "--reverse", "--no-patch", "--format=%an")
	if err == nil {
		parts := strings.Split(strings.TrimSpace(s), "\n")
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			return strings.TrimSpace(parts[0])
		}
	}
	s2, err2 := runGit(repoDir, "log", "--follow", "--reverse", "--format=%an", "--", file)
	if err2 == nil {
		parts := strings.Split(strings.TrimSpace(s2), "\n")
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			return strings.TrimSpace(parts[0])
		}
	}
	return ""
}

func gitFirstTime(repoDir, file string, line int) string {
	if line <= 0 {
		return ""
	}
	s, err := runGit(repoDir, "log", "-L", fmt.Sprintf("%d,%d:%s", line, line, file), "--reverse", "--no-patch", "--format=%aI")
	if err == nil {
		parts := strings.Split(strings.TrimSpace(s), "\n")
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			v := strings.TrimSpace(parts[0])
			if t, ok := parseTimeFlexible(v); ok {
				loc, e := time.LoadLocation("Asia/Shanghai")
				if e != nil {
					loc = time.FixedZone("CST", 8*3600)
				}
				return t.In(loc).Format("2006-01-02 15:04:05")
			}
			return v
		}
	}
	s2, err2 := runGit(repoDir, "log", "--follow", "--reverse", "--format=%aI", "--", file)
	if err2 == nil {
		parts := strings.Split(strings.TrimSpace(s2), "\n")
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			v := strings.TrimSpace(parts[0])
			if t, ok := parseTimeFlexible(v); ok {
				loc, e := time.LoadLocation("Asia/Shanghai")
				if e != nil {
					loc = time.FixedZone("CST", 8*3600)
				}
				return t.In(loc).Format("2006-01-02 15:04:05")
			}
			return v
		}
	}
	return ""
}

func parseTimeFlexible(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
func guessRespType(reqType string, structs map[string]StructInfo, goSources map[string]string) string {
	if reqType == "" {
		return ""
	}
	for _, suf := range []string{"Resp", "Rsp"} {
		cand := reqType + suf
		if _, ok := structs[cand]; ok {
			return cand
		}
	}
	base := reqType
	if strings.HasSuffix(base, "Req") {
		base = base[:len(base)-3]
	}
	for _, suf := range []string{"Resp", "Rsp"} {
		cand := base + suf
		if _, ok := structs[cand]; ok {
			return cand
		}
	}
	return ""
}

// parse proto categories and display names per rules
func parseProtoWithCategories(protoText string) ([]map[string]string, []string, string) {
	lines := strings.Split(protoText, "\n")
	svc := ""
	svcRe := regexp.MustCompile(`\bservice\s+(\w+)\s*\{`)
	rpcRe := regexp.MustCompile(`\brpc\s+(\w+)\s*\(([^)]*)\)\s*returns\s*\(([^)]*)\)`)
	closeRe := regexp.MustCompile(`^\s*\}\s*(//\s*(.*))?$`)
	type rpcItem struct {
		name        string
		req         string
		resp        string
		start       int
		end         int
		lineTail    string
		closeTail   string
		sectionHint string
	}
	var items []rpcItem
	currentSection := ""
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t")
		if m := svcRe.FindStringSubmatch(line); len(m) > 1 {
			svc = m[1]
		}
		// section comment candidate
		if strings.HasPrefix(strings.TrimSpace(line), "//") && !strings.Contains(line, "rpc") {
			commentText := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "//"))
			if !strings.HasPrefix(strings.ToLower(commentText), "import") {
				currentSection = commentText
			}
			continue
		}
		// blank line breaks section group
		if strings.TrimSpace(line) == "" {
			currentSection = ""
			continue
		}
		// skip commented out lines
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		m := rpcRe.FindStringSubmatch(line)
		if len(m) >= 4 {
			name := m[1]
			reqT := strings.TrimSpace(m[2])
			respT := strings.TrimSpace(m[3])
			lineTail := ""
			if tm := regexp.MustCompile(`//\s*(.*)$`).FindStringSubmatch(line); len(tm) > 1 {
				lineTail = strings.TrimSpace(tm[1])
			}
			// section hint from latest section comment until blank line or next comment
			sectionHint := currentSection
			end := i
			closeTail := ""
			if strings.Contains(line, "{") && !strings.HasSuffix(strings.TrimSpace(line), "}") {
				j := i + 1
				for j < len(lines) {
					cm := closeRe.FindStringSubmatch(strings.TrimRight(lines[j], " \t"))
					if len(cm) > 0 {
						end = j
						if len(cm) > 2 && closeTail == "" {
							closeTail = strings.TrimSpace(cm[2])
						}
						break
					}
					j++
				}
			}
			items = append(items, rpcItem{name: name, req: reqT, resp: respT, start: i, end: end, lineTail: lineTail, closeTail: closeTail, sectionHint: sectionHint})
		}
	}
	// count RPCs under same section hint and keep order when first seen
	countBySection := map[string]int{}
	sectionOrder := []string{}
	seenSec := map[string]bool{}
	for _, it := range items {
		if it.sectionHint != "" {
			countBySection[it.sectionHint] = countBySection[it.sectionHint] + 1
			if !seenSec[it.sectionHint] {
				sectionOrder = append(sectionOrder, it.sectionHint)
				seenSec[it.sectionHint] = true
			}
		}
	}
	var methods []map[string]string
	for _, it := range items {
		cat := "未分类"
		if it.sectionHint != "" && countBySection[it.sectionHint] > 1 {
			cat = it.sectionHint
		}
		disp := it.lineTail
		if disp == "" {
			disp = it.closeTail
		}
		if disp == "" && it.sectionHint != "" && countBySection[it.sectionHint] == 1 {
			pc := strings.TrimSpace(it.sectionHint)
			if !strings.HasPrefix(strings.ToLower(pc), "rpc ") {
				disp = it.sectionHint
			}
		}
		if disp == "" {
			disp = it.name
		}
		methods = append(methods, map[string]string{"name": it.name, "req_type": it.req, "resp_type": it.resp, "display_name": disp, "category": cat, "line": fmt.Sprintf("%d", it.start+1)})
	}
	return methods, sectionOrder, svc
}

var indexTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>项目文档导航</title>
  <style>
    :root { color-scheme: light dark; }
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; margin: 0; padding: 24px; line-height: 1.6; background: #f8fafc; }
    header { display: grid; grid-template-columns: 1fr auto; align-items: center; gap: 12px; padding: 12px 0; }
    .brandwrap { display: inline-flex; align-items: center; gap: 12px; }
    .brand { font-weight: 700; font-size: 18px; color: #0f172a; }
    .search { display: flex; gap: 8px; align-items: center; justify-content: flex-end; }
    .search input { width: 260px; max-width: 60vw; padding: 8px 10px; border: 1px solid #d1d5db; border-radius: 8px; outline: none; font-size: 14px; }
    .meta { color: #64748b; font-size: 12px; }
    .readmebtn { display: inline-block; padding: 8px 10px; border-radius: 8px; border: 1px solid #d1d5db; color: #2563eb; background: #e0e7ff; text-decoration: none; font-size: 12px; }
    .readmebtn:hover { background: #dbeafe; }
    .grid { list-style: none; padding: 0; margin: 16px 0 0; display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 12px; }
    .card { background: #fff; border: 1px solid #e5e7eb; border-radius: 10px; padding: 14px; box-shadow: 0 1px 3px rgba(15,23,42,0.06); transition: box-shadow .2s ease, transform .2s ease; cursor: pointer; }
    .card:hover { box-shadow: 0 6px 18px rgba(15,23,42,0.1); transform: translateY(-2px); }
    .card a { display: block; color: #0f172a; text-decoration: none; font-weight: 600; font-size: 14px; }
    .card .sub { margin-top: 6px; color: #64748b; font-size: 12px; }
    .homebtn { position: fixed; top: 12px; left: 12px; background: #4b8bf4; color: #fff; border: none; border-radius: 16px; padding: 6px 10px; font-size: 12px; text-decoration: none; box-shadow: 0 2px 6px rgba(0,0,0,0.2); }
  </style>
</head>
<body>
  {homebtn}
  <header>
    <div class="brandwrap"><div class="brand">{brand}</div><a class="readmebtn" href="{readme_link}">README说明</a></div>
    <div class="search">
      <input id="search" type="text" placeholder="搜索项目" />
      <span class="meta" id="count"></span>
    </div>
  </header>
  <ul id="list" class="grid">{items}</ul>
  <script>
    window.addEventListener('DOMContentLoaded', function(){
        const homeBtn = document.querySelector('.homebtn');
        if (homeBtn && window.GROUP_INDEX) {
            homeBtn.href = window.GROUP_INDEX;
        }
    });
    const input = document.getElementById('search');
    const list = document.getElementById('list');
    const countEl = document.getElementById('count');
    function updateCount(){ const visible = Array.from(list.children).filter(li => li.style.display !== 'none').length; countEl.textContent = '项目数：' + visible; }
    function fuzzy(q,t){ q = (q||'').toLowerCase().replace(/\s+/g,''); t = (t||'').toLowerCase(); if(!q) return true; let i=0; for(let j=0;j<t.length && i<q.length;j++){ if(t[j]===q[i]) i++; } return i===q.length; }
    function filter(){ const q = input.value || ''; Array.from(list.children).forEach(li => { const a = li.querySelector('a'); const t = (a.textContent||''); li.style.display = fuzzy(q,t) ? '' : 'none'; }); updateCount(); }
    updateCount(); input.addEventListener('input', filter);
    Array.from(list.children).forEach(li => { li.classList.add('card'); const a = li.querySelector('a'); const sub = document.createElement('div'); sub.className='sub'; sub.textContent='点击进入项目文档'; li.appendChild(sub); li.setAttribute('tabindex','0'); li.addEventListener('click', ()=>{ if(a && a.href) window.location.href=a.href; }); li.addEventListener('keypress', (e)=>{ if((e.key==='Enter'||e.key===' ') && a && a.href){ e.preventDefault(); window.location.href=a.href; } }); });
  </script>
</body>
</html>`

var fullPageTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>API 文档</title>
  <style>
    :root { color-scheme: light dark; }
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; margin: 0; padding: 64px 0 24px; line-height: 1.6; background: #f8fafc; }
    .container { max-width: 1200px; margin: 0; padding: 0 24px; padding-left: calc(16px + 280px); }
    header { margin-bottom: 24px; }
    h1 { font-size: 22px; margin: 0 0 8px; }
    .desc { color: #64748b; font-size: 14px; margin-bottom: 4px; }
    .homebtn { position: fixed; top: 12px; left: 12px; background: #4b8bf4; color: #fff; border: none; border-radius: 16px; padding: 6px 10px; font-size: 12px; text-decoration: none; box-shadow: 0 2px 6px rgba(0,0,0,0.2); }
    .section { margin: 24px 0; }
    .section h2 { font-size: 18px; margin: 0 0 12px; border-left: 3px solid #4b8bf4; padding-left: 8px; }
    .rpc { border: 1px solid #e5e7eb; border-radius: 10px; padding: 14px; margin: 12px 0; background: #fff; box-shadow: 0 1px 3px rgba(15,23,42,0.06); transition: box-shadow .2s ease, transform .2s ease; }
    .rpc:hover { box-shadow: 0 6px 18px rgba(15,23,42,0.08); transform: translateY(-1px); }
    .rpc-title { font-weight: 600; color: #0f172a; }
    .badge { display: inline-block; font-size: 11px; border-radius: 6px; padding: 2px 6px; margin-left: 8px; border: 1px solid transparent; }
    .badge-new { color: #fff; background: #ef4444; border-color: #dc2626; }
    .badge-mod { color: #fff; background: #ef4444; border-color: #dc2626; }
    .diff-tag { color: #ef4444; font-weight: 600; }
    .rpc-title.ghost { opacity: 0; }
    .rpc-meta { font-family: ui-monospace, Menlo, Monaco, Consolas, "Courier New", monospace; font-size: 13px; color: #334155; }
    .rpc-desc { font-size: 14px; color: #475569; }
    .code { font-family: ui-monospace, Menlo, Monaco, Consolas, "Courier New"; background: #f1f5f9; border-radius: 6px; padding: 2px 6px; }
    .layout { display: grid; grid-template-columns: 280px minmax(720px, 1fr) clamp(300px, 26vw, 480px); gap: 24px; padding-left: 16px; padding-right: 16px; align-items: start; }
    .sidebar { position: sticky; top: 64px; align-self: start; background:#fff; height: calc(100vh - 64px); overflow-y: auto; padding-right: 4px; }
    .rightbar { position: sticky; top: 64px; align-self: start; background:#fff; height: calc(100vh - 64px); overflow-y: auto; padding-left: 8px; }
    .content { min-width: 720px; }
    .nav { display: block; }
    .navblock { margin-bottom: 8px; }
    .navlinks { display: grid; grid-template-columns: 1fr; gap: 6px; }
    .navblock .rpc-title { cursor: pointer; display: flex; align-items: center; gap: 10px; font-weight: 600; color: #0f172a; padding: 8px 10px; border-radius: 10px; background: #fff; border: 1px solid #e5e7eb; }
    .navblock .rpc-title:hover { background: #f3f4f6; }
    .navblock .rpc-title.ghost { display: none; }
    .navblock .rpc-title::before { content: '▾'; font-size: 12px; color: #64748b; margin-right: 6px; }
    .navblock.collapsed .rpc-title::before { content: '▸'; }
    .navblock.collapsed .navlinks { display: none; }
    .navlinks a.item { display: flex; align-items: center; gap: 8px; padding: 6px 10px; border-radius: 8px; text-decoration: none; transition: background .15s ease; flex-wrap: wrap; background: transparent; border: none; }
    .navlinks a.item:hover { background: #eef2ff; }
    .navlinks a.item.active { background: #e0e7ff; }
    .navlinks .idx { display: inline-flex; align-items: center; justify-content: center; min-width: 22px; height: 22px; border-radius: 11px; background: #e0e7ff; color: #1e3a8a; font-size: 12px; font-weight: 600; }
    .navlinks .en { font-weight: 600; color: #0f172a; overflow-wrap: anywhere; }
    .navlinks .sep { color: #94a3b8; margin: 0 4px; }
    .navlinks .zh { color: #475569; overflow-wrap: anywhere; }
    table { width: 100%; border-collapse: collapse; margin: 8px 0; background: #fff; }
    th, td { border: 1px solid #e5e7eb; padding: 8px 10px; text-align: left; }
    thead th { background: #f3f4f6; font-weight: 600; color: #0f172a; }
    .codebox { position: relative; border: 1px solid #e5e7eb; background: #f7f8fa; border-radius: 8px; padding: 12px; display: flex; flex-direction: column; padding-top: 34px; overflow: hidden; }
    .copybtn { position: absolute; top: 8px; right: 8px; font-size: 12px; color: #2563eb; cursor: pointer; background: #e0e7ff; border-radius: 12px; padding: 2px 8px; }
    .sendbtn { position: absolute; top: 8px; right: 60px; font-size: 12px; color: #0ea5e9; cursor: pointer; background: #cffafe; border-radius: 12px; padding: 2px 8px; }
    .modal-mask { position: fixed; inset: 0; background: rgba(0,0,0,0.35); display: none; align-items: center; justify-content: center; z-index: 10000; }
    .modal { width: min(1000px, 90vw); height: 80vh; max-height: 80vh; background: #fff; border-radius: 12px; box-shadow: 0 10px 30px rgba(0,0,0,0.2); padding: 14px; display: flex; flex-direction: column; box-sizing: border-box; }
    .modal h3 { margin: 0 0 10px; font-size: 16px; }
    .modal-body { flex: 1; display: grid; grid-template-columns: 1fr 1fr; gap: 12px; align-items: stretch; overflow: hidden; }
    .modal-left { overflow: auto; padding-right: 6px; }
    .modal-right { display: flex; flex-direction: column; overflow: auto; }
    .form-row { display: flex; gap: 10px; margin-bottom: 10px; align-items: center; }
    .form-block { display: flex; flex-direction: column; align-items: stretch; }
    .form-block.stretch { flex: 1; }
    .form-block.stretch .codebox { flex: 1; height: 50vh; }
    .form-row label { width: 80px; color: #334155; }
    .form-row select, .form-row input { flex: 1; padding: 6px 8px; border: 1px solid #e5e7eb; border-radius: 8px; }
    .jsonarea { width: 100%; border: 1px solid #e5e7eb; border-radius: 8px; padding: 8px; font-family: ui-monospace, Menlo, Monaco, Consolas, "Courier New"; flex: 1; box-sizing: border-box; }
    .codebox .rpc-meta, .codebox .jsonarea { background: #f7f8fa; border: 0; margin: 0; padding: 0; width: 100%; height: 100%; }
    .codebox .rpc-meta, .codebox .jsonarea { overflow: auto; }
    .codebox .rpc-meta::-webkit-scrollbar, .codebox .jsonarea::-webkit-scrollbar { width: 8px; height: 8px; }
    .codebox .rpc-meta::-webkit-scrollbar-track, .codebox .jsonarea::-webkit-scrollbar-track { background: #eef2ff; border-radius: 8px; }
    .codebox .rpc-meta::-webkit-scrollbar-thumb, .codebox .jsonarea::-webkit-scrollbar-thumb { background: #c7d2fe; border-radius: 8px; }
    .form-block.stretch .codebox .jsonarea { height: 100%; min-height: 0; overflow: auto; }
    .jsonarea:focus { outline: none; }
    .modal-actions { display: flex; gap: 8px; justify-content: flex-end; margin-top: 10px; flex-shrink: 0; }
    .btn { padding: 6px 12px; border-radius: 10px; border: 1px solid #e5e7eb; cursor: pointer; }
    .btn.primary { background: #4b8bf4; color: #fff; border-color: #4b8bf4; }
    #invoke_result { flex: 1; height: 100%; max-height: none; overflow-y: auto; overflow-x: auto; box-sizing: border-box; }
    .codebox .rpc-meta { flex: 1; width: 100%; min-height: 0; overflow: auto; }
    .codebox .rpc-meta { flex: 1; width: 100%; min-height: 0; overflow: auto; }
    .toast { position: fixed; top: 20px; right: 20px; background: rgba(34,197,94,.95); color: #fff; padding: 8px 12px; border-radius: 6px; font-size: 13px; z-index: 9999; display: none; box-shadow: 0 4px 12px rgba(0,0,0,0.15); }
    .copytip { position: absolute; top: -22px; right: 8px; background: rgba(34,197,94,.95); color: #fff; padding: 2px 6px; border-radius: 10px; font-size: 12px; display: none; }
    .jsonviewer { font-family: ui-monospace, Menlo, Monaco, Consolas, "Courier New"; font-size: 13px; color: #334155; }
    .jsonviewer details { margin-left: 12px; }
    .jsonviewer summary { cursor: pointer; list-style: none; display: flex; align-items: center; gap: 6px; }
    .jsonviewer .caret { display: inline-block; width: 10px; color: #64748b; }
    .jsonviewer details[open] > summary .caret { content: '▾'; }
    .jsonviewer summary .caret::before { content: '▸'; }
    .jsonviewer details[open] > summary .caret::before { content: '▾'; }
    .jsonviewer .json-key { color: #0f172a; font-weight: 600; }
    .jsonviewer .json-type { color: #64748b; font-size: 12px; }
    .jsonviewer .json-value-string { color: #059669; }
    .jsonviewer .json-value-number { color: #2563eb; }
    .jsonviewer .json-value-boolean { color: #d97706; }
    .jsonviewer .json-value-null { color: #6b7280; }
  </style>
  <script>
    function copyToClipboard(text, btn) {
        if (navigator.clipboard && navigator.clipboard.writeText) {
            navigator.clipboard.writeText(text).then(() => showCopyTip(btn)).catch(err => fallbackCopy(text, btn));
        } else {
            fallbackCopy(text, btn);
        }
    }
    function fallbackCopy(text, btn) {
        const ta = document.createElement('textarea');
        ta.value = text;
        ta.style.position = 'fixed';
        ta.style.left = '-9999px';
        document.body.appendChild(ta);
        ta.select();
        try {
            document.execCommand('copy');
            showCopyTip(btn);
        } catch (e) {
            alert('复制失败');
        }
        document.body.removeChild(ta);
    }
    async function load() {
      try {
        const res = await fetch('__DOC_JSON__');
        const data = await res.json();
        document.getElementById('service').textContent = data.service;
        const svc = (data.service || 'Service.Service');
        const svcName = (svc.includes('.') ? svc.split('.')[0] : svc);
        const titleText = svcName + ' 服务 API 文档';
        document.getElementById('page_title').textContent = titleText;
        document.title = titleText;
        document.getElementById('proto').textContent = data.proto;
        const apiDirLink = document.getElementById('api_dir_link');
        apiDirLink.href = 'file://' + data.api_dir;
        apiDirLink.textContent = data.api_dir;
        const nav = document.getElementById('quick_nav');
        const container = document.getElementById('methods');
        nav.innerHTML = ''; container.innerHTML = '';
        const statsBox = document.getElementById('stats');
        const devBox = document.getElementById('devstats');
        const changesBox = document.getElementById('changes');
        const stats = data.stats || {};
        window.__ENV_OPTS__ = (stats.envs || []);
        const total = stats.total || (data.methods||[]).length;
        const categories = stats.categories || [];
        const catTotal = stats.category_total || categories.length;
        const developers = stats.developers || [];
        const devTotal = stats.developer_total || developers.length;
        if (statsBox) { const statMeta = document.createElement('div'); statMeta.className = 'rpc-desc'; statMeta.textContent = '接口总数：' + total + '，分类总数：' + catTotal; statsBox.appendChild(statMeta); }
        const sideStats = document.getElementById('side_stats'); if (sideStats) { sideStats.textContent = '接口总数：' + total + '，分类总数：' + catTotal; }
        if (changesBox) {
          const changes = (data.methods||[]).filter(m=>m.diff==='NEW'||m.diff==='MODIFIED');
          if (changes.length) {
            const tbl = document.createElement('table');
            const thead = document.createElement('thead'); thead.innerHTML = '<tr><th>接口</th><th>变更</th></tr>'; tbl.appendChild(thead);
            const tbody = document.createElement('tbody');
            changes.forEach(m=>{ const tr = document.createElement('tr'); const nm = (m.display_name||m.name); const badge = '<span class="badge '+(m.diff==='NEW'?'badge-new':'badge-mod')+'">'+m.diff+'</span>'; tr.innerHTML = '<td><a href="#'+m.name+'">'+nm+'</a></td><td>'+badge+'</td>'; tbody.appendChild(tr); });
            tbl.appendChild(tbody);
            changesBox.appendChild(tbl);
          } else {
            const meta = document.createElement('div'); meta.className='rpc-desc'; meta.textContent='无变更'; changesBox.appendChild(meta);
          }
        }
        if (statsBox && categories.length) {
          const tbl = document.createElement('table'); const thead = document.createElement('thead'); thead.innerHTML = '<tr><th>分类</th><th>数量</th></tr>'; tbl.appendChild(thead);
          const tbody = document.createElement('tbody'); categories.forEach(c => { const tr = document.createElement('tr'); tr.innerHTML = '<td><a href="#cat-' + encodeURIComponent(c.name) + '\">' + c.name + '</a></td><td>' + c.count + '</td>'; tbody.appendChild(tr); }); tbl.appendChild(tbody); statsBox.appendChild(tbl);
        }
        if (devBox && developers.length) {
          const dtbl = document.createElement('table'); const dthead = document.createElement('thead'); dthead.innerHTML = '<tr><th>名字</th><th>创建数量</th><th>更新数量</th></tr>'; dtbl.appendChild(dthead);
          const dtbody = document.createElement('tbody'); developers.forEach(dv => { const tr = document.createElement('tr'); tr.innerHTML = '<td>' + dv.name + '</td><td>' + (dv.created||0) + '</td><td>' + (dv.updated||0) + '</td>'; dtbody.appendChild(tr); }); dtbl.appendChild(dtbody); devBox.appendChild(dtbl);
        }
        const catOrder = (data.stats && data.stats.categories) ? data.stats.categories.map(c=>c.name) : [];
        const byCat = {}; (data.methods||[]).forEach(m => { const cat = m.category || '未分类'; if (!byCat[cat]) byCat[cat] = []; byCat[cat].push(m); });
        const orderedCats = catOrder.length ? catOrder : Object.keys(byCat);
        orderedCats.forEach(catName => {
          const items = (byCat[catName] || []);
          for (let start = 0; start < items.length; start += 10) {
            const block = document.createElement('div'); block.className = 'navblock'; block.setAttribute('data-cat', catName);
            const title = document.createElement('div'); title.className = 'rpc-title' + (start === 0 ? '' : ' ghost'); title.textContent = '📁 ' + catName + ' (' + items.length + ')'; block.appendChild(title);
            const list = document.createElement('div'); list.className = 'navlinks';
            items.slice(start, start + 10).forEach(m => {
              const id = m.name; const zh = (m.display_name || ''); const en = m.name;
              const link = document.createElement('a'); link.href = '#' + id; link.className = 'item';
              const idx = document.createElement('span'); idx.className = 'idx'; idx.textContent = (m.seq? m.seq : '');
              const enEl = document.createElement('span'); enEl.className = 'en'; enEl.textContent = en;
              const sep = document.createElement('span'); sep.className = 'sep'; sep.textContent = ' -- ';
              const zhEl = document.createElement('span'); zhEl.className = 'zh'; zhEl.textContent = (zh && zh !== en) ? zh : '';
              link.appendChild(idx); link.appendChild(enEl);
              if (zh && zh !== en) { link.appendChild(sep); link.appendChild(zhEl); }
              if (m.diff === 'NEW' || m.diff === 'MODIFIED') { const b = document.createElement('span'); b.className = 'badge ' + (m.diff==='NEW'?'badge-new':'badge-mod'); b.textContent = m.diff; link.appendChild(b); }
              list.appendChild(link);
            });
            block.appendChild(list); nav.appendChild(block);
          }
        });
        const collapsedState = (function(){ try{ return JSON.parse(localStorage.getItem('woda_nav_collapsed')||'{}'); }catch(e){ return {}; } })();
        function setCatCollapsed(cat, collapsed){ const blocks = Array.from(nav.querySelectorAll('.navblock')).filter(b=>b.getAttribute('data-cat')===cat); blocks.forEach(b=>{ if(collapsed){ b.classList.add('collapsed'); } else { b.classList.remove('collapsed'); } }); }
        orderedCats.forEach(catName => { const c = Object.prototype.hasOwnProperty.call(collapsedState, catName) ? !!collapsedState[catName] : true; setCatCollapsed(catName, c); });
        Array.from(nav.querySelectorAll('.navblock')).forEach(b=>{ const t = b.querySelector('.rpc-title'); if(t && !t.classList.contains('ghost')){ t.addEventListener('click', function(){ const cat = b.getAttribute('data-cat')||''; const isC = b.classList.contains('collapsed'); const next = !isC; setCatCollapsed(cat, next); collapsedState[cat] = next; try{ localStorage.setItem('woda_nav_collapsed', JSON.stringify(collapsedState)); }catch(e){} }); } });
        function highlightCurrentNav(){ const id = (location.hash||'').replace('#',''); Array.from(nav.querySelectorAll('a.item')).forEach(a => { a.classList.toggle('active', a.getAttribute('href') === ('#'+id)); }); }
        window.addEventListener('hashchange', highlightCurrentNav);
        highlightCurrentNav();
        orderedCats.forEach(catName => {
          const catTitle = document.createElement('div'); catTitle.className = 'rpc-title'; catTitle.id = 'cat-' + encodeURIComponent(catName); catTitle.textContent = catName; container.appendChild(catTitle);
          (byCat[catName] || []).forEach(m => {
            const id = m.name; const display = (m.display_name || m.name);
            const card = document.createElement('div'); card.className = 'rpc'; card.id = id; const title = document.createElement('div'); title.className = 'rpc-title'; title.textContent = ((m.seq? (m.seq + '. ') : '') + display); if (m.diff==='NEW'||m.diff==='MODIFIED'){ const b=document.createElement('span'); b.className='badge '+(m.diff==='NEW'?'badge-new':'badge-mod'); b.textContent=m.diff; title.appendChild(b);} card.appendChild(title);
            card.setAttribute('data-name', id); card.setAttribute('data-display', display);
            const reqMeta = document.createElement('div'); reqMeta.className = 'rpc-desc'; reqMeta.innerHTML = '请求方式：<b>' + (m.method || 'gRPC（Unary）') + '</b>'; card.appendChild(reqMeta);
            const meta = document.createElement('div'); meta.className = 'rpc-meta'; meta.textContent = '请求地址：' + m.grpc_path; card.appendChild(meta);
            const authors = document.createElement('div'); authors.className = 'rpc-desc'; authors.innerHTML = '接口创建人：' + (m.git_created_by || '') + (m.git_created_at ? ('（' + m.git_created_at + '）') : '') + '<br/>最后修改人：' + (m.git_last_modified || '') + (m.git_last_modified_at ? ('（' + m.git_last_modified_at + '）') : ''); card.appendChild(authors);
            const reqExTitle = document.createElement('div'); reqExTitle.className = 'rpc-title'; reqExTitle.textContent = '请求示例：'; card.appendChild(reqExTitle);
            const reqCode = document.createElement('div'); reqCode.className = 'codebox'; const copy = document.createElement('span'); copy.className = 'copybtn'; copy.textContent = '复制'; copy.addEventListener('click', () => { copyToClipboard(JSON.stringify(m.request || {}, null, 2), copy); }); const reqPreBlock = document.createElement('pre'); reqPreBlock.className = 'rpc-meta'; reqPreBlock.textContent = JSON.stringify(m.request || {}, null, 2); const send = document.createElement('span'); send.className = 'sendbtn'; send.textContent = '发送请求'; reqCode.appendChild(copy); reqCode.appendChild(send); reqCode.appendChild(reqPreBlock); card.appendChild(reqCode);
            const reqTitle = document.createElement('div'); reqTitle.className = 'rpc-title'; reqTitle.textContent = '参数说明'; card.appendChild(reqTitle);
            const reqTable = document.createElement('table'); const reqThead = document.createElement('thead'); reqThead.innerHTML = '<tr><th>参数</th><th>必须</th><th>说明</th></tr>'; reqTable.appendChild(reqThead); const reqTbody = document.createElement('tbody'); const isReq = c => /必填|必选|必须/.test(c||''); const nf = new Set(m.new_fields||[]); const mf = new Set(m.modified_fields||[]); (m.request_fields||[]).forEach(f => { const tr = document.createElement('tr'); let tail = ''; if(nf.has(f.name)){ tail = ' <span class="diff-tag">[NEW]</span>'; } else if (mf.has(f.name)){ tail = ' <span class="diff-tag">[MODIFIED]</span>'; } tr.innerHTML = '<td>' + f.name + tail + '</td><td>' + ((f.required? '是' : (isReq(f.comment)? '是' : '否'))) + '</td><td>' + (f.comment||'') + '</td>'; reqTbody.appendChild(tr); }); reqTable.appendChild(reqTbody); card.appendChild(reqTable);
            const respTitle = document.createElement('div'); respTitle.className = 'rpc-title'; respTitle.textContent = '返回结果'; card.appendChild(respTitle);
            const respCode = document.createElement('div'); respCode.className = 'codebox'; const rcopy = document.createElement('span'); rcopy.className = 'copybtn'; rcopy.textContent = '复制'; rcopy.addEventListener('click', () => { copyToClipboard(JSON.stringify(m.response_envelope || {}, null, 2), rcopy); }); const respPre = document.createElement('pre'); const respPreId = 'resp_'+id; respPre.id = respPreId; respPre.className = 'rpc-meta'; respPre.textContent = JSON.stringify(m.response_envelope || {}, null, 2); respCode.appendChild(rcopy); respCode.appendChild(respPre); card.appendChild(respCode);
            send.addEventListener('click', () => { openInvoke({ method: m.name, service: svcName, initial: m.request || {}, respPreId }); });
            const respParamTitle = document.createElement('div'); respParamTitle.className = 'rpc-title'; respParamTitle.textContent = '参数说明'; card.appendChild(respParamTitle);
            const respTable = document.createElement('table'); const respThead = document.createElement('thead'); respThead.innerHTML = '<tr><th>参数</th><th>说明</th></tr>'; respTable.appendChild(respThead); const respTbody = document.createElement('tbody'); const nrf = new Set(m.new_resp_fields||[]); const mrf = new Set(m.modified_resp_fields||[]); (m.response_fields||[]).forEach(f => { const tr = document.createElement('tr'); let tail = ''; if(nrf.has(f.name)){ tail = ' <span class="diff-tag">[NEW]</span>'; } else if (mrf.has(f.name)){ tail = ' <span class="diff-tag">[MODIFIED]</span>'; } tr.innerHTML = '<td>' + f.name + tail + '</td><td>' + (f.comment||'') + '</td>'; respTbody.appendChild(tr); }); respTable.appendChild(respTbody); card.appendChild(respTable);
            container.appendChild(card);
          });
        });
      } catch (e) {}
    }
    async function loadBranches() {
      try {
        const prefix = window.PATH_PREFIX || '../';
        const res = await fetch(prefix + 'branches.json');
        const branches = await res.json();
        const sel = document.getElementById('branch_selector');
        if (!sel || !branches || !branches.length) return;
        const current = window.CURRENT_BRANCH || 'master';
        branches.forEach(b => {
          const opt = document.createElement('option');
          // Support new structure {name: "...", date: "..."} or old string "..."
          const bName = (typeof b === 'object' && b.name) ? b.name : b;
          const bDate = (typeof b === 'object' && b.date) ? b.date : '';
          
          opt.value = bName;
          opt.textContent = bName + (bDate ? (' (' + bDate + ')') : '');
          if (bName === current) opt.selected = true;
          sel.appendChild(opt);
        });
        sel.addEventListener('change', function() {
          const target = sel.value;
          if (target && target !== current) {
             window.location.href = prefix + target + '/index.html';
          }
        });
      } catch(e) { console.error('Failed to load branches', e); }
    }
    // modal for invoke
    let INVOKE_STATE = null;
    function openInvoke(ctx){
      const mask = document.getElementById('invoke_mask'); mask.style.display='flex'; INVOKE_STATE = ctx || {};
      const envs = (window.__ENV_OPTS__||[]);
      const envSel = document.getElementById('invoke_env'); envSel.innerHTML=''; envs.forEach(e=>{ const o=document.createElement('option'); o.value=e; o.textContent=e; envSel.appendChild(o); });
      const cachedEnv = localStorage.getItem('woda_env'); envSel.value = (cachedEnv && envs.includes(cachedEnv)) ? cachedEnv : (envs[0]||''); envSel.onchange = function(){ localStorage.setItem('woda_env', envSel.value||''); };
      const uidEl = document.getElementById('invoke_uid'); const cached = localStorage.getItem('woda_uid'); uidEl.value = cached ? cached : '0';
      document.getElementById('invoke_service').textContent = ctx.service || '';
      document.getElementById('invoke_method').textContent = ctx.method || '';
      (function(){ let init = ctx.initial || {}; if (Object.prototype.hasOwnProperty.call(init, 'RecordSize')) { let n = Number(init.RecordSize); if (!(n > 0)) init.RecordSize = 10; } document.getElementById('invoke_json').textContent = JSON.stringify(init, null, 2); })();
      const r = document.getElementById('invoke_result'); if(r){ r.innerHTML = ''; }
      populateReqHistory();
      document.body.style.overflow = 'hidden';
    }
    function showToast(msg){ const m = document.querySelector('#invoke_mask .toast'); if(!m) return; m.textContent = msg||'复制成功'; m.style.display='block'; setTimeout(()=>{ m.style.display='none'; }, 1200); }
    function showCopyTip(btn){ 
        let tip = document.getElementById('global_copy_tip');
        if(!tip){ 
            tip = document.createElement('span'); 
            tip.id = 'global_copy_tip';
            tip.className='copytip'; 
            tip.textContent='复制成功'; 
            document.body.appendChild(tip); 
        }
        const rect = btn.getBoundingClientRect();
        tip.style.position = 'fixed';
        tip.style.zIndex = '10000';
        tip.style.top = (rect.top - 30) + 'px';
        // Align right edge of tip with right edge of button to avoid overflow
        // Use right property relative to viewport width
        tip.style.left = 'auto';
        tip.style.right = (document.documentElement.clientWidth - rect.right) + 'px';
        
        tip.style.display='block'; 
        setTimeout(()=>{ tip.style.display='none'; }, 1200); 
    }
    let INVOKE_LAST_OBJ = null;
    function historyKey(){ const s = (INVOKE_STATE && INVOKE_STATE.service) ? INVOKE_STATE.service : ''; const m = (INVOKE_STATE && INVOKE_STATE.method) ? INVOKE_STATE.method : ''; return 'woda_req_history:' + s + ':' + m; }
    function getReqHistory(){ try{ return JSON.parse(localStorage.getItem(historyKey())||'[]'); }catch(e){ return []; } }
    function saveReqHistory(arr){ try{ localStorage.setItem(historyKey(), JSON.stringify(arr)); }catch(e){} }
    function addReqHistory(entry){ let arr = getReqHistory(); arr = [entry].concat(arr.filter(e => JSON.stringify(e) !== JSON.stringify(entry))).slice(0,10); saveReqHistory(arr); }
    function populateReqHistory(){ const sel = document.getElementById('invoke_history'); if(!sel) return; sel.innerHTML=''; const arr = getReqHistory(); arr.forEach((e, i) => { const opt = document.createElement('option'); opt.value = String(i); opt.textContent = '记录 ' + (i+1); sel.appendChild(opt); }); sel.onchange = function(){ fillFromHistory(); }; if(arr.length){ sel.selectedIndex = arr.length - 1; fillFromHistory(); } }
    function fillFromHistory(){ const sel = document.getElementById('invoke_history'); if(!sel) return; const idx = parseInt(sel.value||'-1',10); const arr = getReqHistory(); if(!(idx>=0) || idx>=arr.length) return; const item = arr[idx]; const envSel = document.getElementById('invoke_env'); if(envSel){ envSel.value = item.env||envSel.value; }
      const uidEl = document.getElementById('invoke_uid'); if(uidEl){ uidEl.value = String(item.uid||0); }
      const jsonEl = document.getElementById('invoke_json'); if(jsonEl){ try{ const obj = typeof item.payload === 'string' ? JSON.parse(item.payload||'{}') : (item.payload||{}); jsonEl.textContent = JSON.stringify(obj, null, 2); }catch(e){ jsonEl.textContent = item.payload||''; } }
    }
    function renderJsonViewer(data, container){ if(!container){ return; } container.innerHTML=''; function makeNode(key, value){ const t = typeof value; if(value && (t==='object')){ const isArr = Array.isArray(value); const det = document.createElement('details'); det.open = true; const sum = document.createElement('summary'); const caret = document.createElement('span'); caret.className='caret'; const k = document.createElement('span'); k.className='json-key'; k.textContent = key !== null ? key + ':' : (isArr? '[]' : '{}'); const ty = document.createElement('span'); ty.className='json-type'; ty.textContent = isArr? (' Array(' + value.length + ')') : ' Object'; sum.appendChild(caret); sum.appendChild(k); sum.appendChild(ty); det.appendChild(sum); const wrap = document.createElement('div'); const keys = isArr ? value.map((v, i)=>[String(i), v]) : Object.keys(value).map(k2=>[k2, value[k2]]); keys.forEach(([ck, cv])=>{ wrap.appendChild(makeNode(ck, cv)); }); det.appendChild(wrap); return det; } else { const line = document.createElement('div'); const k = document.createElement('span'); k.className='json-key'; if(key!==null){ k.textContent = key + ': '; } const v = document.createElement('span'); let cls = 'json-value-string'; if(t==='number'){ cls='json-value-number'; } else if(t==='boolean'){ cls='json-value-boolean'; } else if(value===null){ cls='json-value-null'; } v.className = cls; v.textContent = t==='string'? ('"' + value + '"') : String(value); line.appendChild(k); line.appendChild(v); return line; } }
      const root = makeNode(null, data); container.appendChild(root); }
    function closeInvoke(){ const mask = document.getElementById('invoke_mask'); mask.style.display='none'; document.body.style.overflow = ''; }
    async function submitInvoke(){
      try{
        const env = document.getElementById('invoke_env').value || '';
        const uid = parseInt(document.getElementById('invoke_uid').value||'0',10)||0;
        let obj; try{ obj = JSON.parse(document.getElementById('invoke_json').textContent||'{}'); }catch(e){ alert('JSON格式错误：'+e.message); return; }
        if (Object.prototype.hasOwnProperty.call(obj, 'RecordSize')) { let n = Number(obj.RecordSize); if (!(n > 0)) obj.RecordSize = 10; }
        const payload = { ConsulName: env, ServiceName: (INVOKE_STATE.service||''), Method: (INVOKE_STATE.method||''), Uid: uid, AppData: JSON.stringify(obj) };
        try{ localStorage.setItem('woda_uid', String(uid)); }catch(e){}
        const res = await fetch('__CONSUL_ADDR__', { method: 'POST', headers: { 'Content-Type':'application/json' }, body: JSON.stringify(payload) });
        const modalPre = document.getElementById('invoke_result');
        const pre = document.getElementById(INVOKE_STATE.respPreId);
        let text = await res.text(); let out; try{ out = JSON.parse(text); }catch(_){ out = null; }
        let showText;
        if(out){
          let d = out.data;
          try{ if(typeof d === 'string'){ d = JSON.parse(d); } }catch(e){}
          showText = JSON.stringify({ code: out.code, message: out.message, data: d }, null, 2);
          INVOKE_LAST_OBJ = { code: out.code, message: out.message, data: d };
          if(modalPre){ renderJsonViewer(INVOKE_LAST_OBJ, modalPre); }
          if(out && Number(out.code) === 0){ addReqHistory({ env, uid, service: (INVOKE_STATE.service||''), method: (INVOKE_STATE.method||''), payload: obj }); populateReqHistory(); }
        } else {
          try{ showText = JSON.stringify(JSON.parse(text), null, 2); }catch(e){ showText = (text||''); }
          INVOKE_LAST_OBJ = null;
          if(modalPre){ modalPre.textContent = showText; }
        }
        if(pre){ pre.textContent = showText; }
      }catch(e){ alert('请求失败：'+e.message); }
    }
    window.addEventListener('DOMContentLoaded', load);
    window.addEventListener('DOMContentLoaded', loadBranches);
    window.addEventListener('DOMContentLoaded', function(){
      const jc = document.getElementById('invoke_json_copy'); if(jc){ jc.addEventListener('click', function(){ const t = document.getElementById('invoke_json'); copyToClipboard(t ? (t.textContent||'') : '', jc); }); }
      const rc = document.getElementById('invoke_result_copy'); if(rc){ rc.addEventListener('click', function(){ const str = INVOKE_LAST_OBJ ? JSON.stringify(INVOKE_LAST_OBJ, null, 2) : (document.getElementById('invoke_result') ? (document.getElementById('invoke_result').textContent||'') : ''); copyToClipboard(str, rc); }); }
      const uidEl = document.getElementById('invoke_uid'); if(uidEl){ const cached = localStorage.getItem('woda_uid'); if(cached){ uidEl.value = cached; } uidEl.addEventListener('input', function(){ localStorage.setItem('woda_uid', uidEl.value||'0'); }); }
      const jsonEl = document.getElementById('invoke_json'); if(jsonEl){
        jsonEl.addEventListener('paste', function(e){
          const cd = e.clipboardData; if(!cd) return; const files = cd.files || []; if(files && files.length){ e.preventDefault(); return; }
          const text = cd.getData('text/plain'); if(text){
            e.preventDefault();
            const sel = window.getSelection();
            if(!sel || sel.rangeCount===0){ jsonEl.textContent = (jsonEl.textContent||'') + text; }
            else { const r = sel.getRangeAt(0); r.deleteContents(); r.insertNode(document.createTextNode(text)); sel.removeAllRanges(); const nr = document.createRange(); nr.selectNodeContents(jsonEl); nr.collapse(false); sel.addRange(nr); }
          }
        });
        jsonEl.addEventListener('drop', function(e){ e.preventDefault(); });
      }
    });
    window.addEventListener('DOMContentLoaded', function(){ const btn = document.getElementById('back_top'); if (btn) { btn.addEventListener('click', function(){ window.scrollTo({top:0, behavior:'smooth'}); }); } });
    window.addEventListener('scroll', function(){ const btn = document.getElementById('back_top'); if (!btn) return; const top = (document.documentElement.scrollTop || document.body.scrollTop || 0); btn.style.display = top > 200 ? 'block' : 'none'; });
  </script>
</head>
<body>
  <a class="homebtn" href="../../index.html">返回上一级</a>
  <div class="container">
  <header>
    <h1 id="page_title"></h1>
    <div class="desc">
      <span style="margin-right:8px;font-weight:600;">分支:</span><select id="branch_selector" style="padding:4px;border-radius:4px;border:1px solid #d1d5db;margin-right:12px;"></select>
      传输协议：gRPC；服务名：<span class="code" id="service"></span>。统一请求/响应：<span class="code">AASMessage.InvokeServiceRequest</span> / <span class="code">AASMessage.InvokeServiceResponse</span>。
    </div>
    <div class="desc">api目录：<a class="code" id="api_dir_link" href="#" target="_blank"></a></div>
    <div class="desc">proto 源文件：<span class="code" id="proto"></span>；文档由脚本自动生成。</div>
  </header>
  </div>
  <div class="layout">
    <aside class="sidebar">
      <div class="section"><h2>目录</h2><div id="side_stats" class="rpc-desc"></div><div id="quick_nav" class="nav"></div></div>
    </aside>
    <main class="content">
      <div class="section"><h2>使用说明</h2><div class="rpc"><div class="rpc-desc">所有接口均为 Unary RPC，入参与出参采用统一封装 <span class="code">AASMessage.InvokeServiceRequest</span> / <span class="code">AASMessage.InvokeServiceResponse</span>。具体封装字段请参阅公司公共协议 <span class="code">common_proto/AASMessage</span>。</div><div class="rpc-desc">gRPC 全路径形如：<span class="code">/{Service}.{Service}/&lt;Method&gt;</span>。</div><div class="rpc-desc">文档内容由 proto 与 Go 结构体自动解析生成，响应示例以通用封装 <span class="code">{code,message,data}</span> 展示。</div></div></div>
      <div class="section"><h2>API 列表</h2><div id="methods"></div></div>
      <footer>文档由 <span class="code">Woda_Doc/gen_doc.go</span> 自动生成的 <span class="code">项目文档数据</span> 渲染，无需手工维护。</footer>
    </main>
    <aside class="rightbar">
      <div class="section"><h2>开发统计</h2><div class="rpc" id="devstats"></div></div>
      <div class="section"><h2>API 变更</h2><div class="rpc" id="changes"></div></div>
    </aside>
  </div>
  <button id="back_top" class="backtop" style="position: fixed; right: 20px; bottom: 20px; background: #4b8bf4; color: #fff; border: none; border-radius: 20px; padding: 8px 12px; font-size: 12px; cursor: pointer; box-shadow: 0 2px 6px rgba(0,0,0,0.2); display: none; z-index: 9999;">返回顶部</button>
  <div id="invoke_mask" class="modal-mask"><div class="modal"><h3>发送请求</h3><div class="modal-body"><div class="modal-left"><div class="form-row"><label>环境</label><select id="invoke_env"></select></div><div class="form-row"><label>UID</label><input id="invoke_uid" type="number" value="0" /></div><div class="form-row"><label>历史记录</label><select id="invoke_history"></select></div><div class="form-row"><label>服务</label><div class="code" id="invoke_service"></div></div><div class="form-row"><label>方法</label><div class="code" id="invoke_method"></div></div><div class="form-row form-block stretch"><label style="width:auto;">请求参数</label><div class="codebox"><span id="invoke_json_copy" class="copybtn">复制</span><pre id="invoke_json" class="rpc-meta jsonarea" contenteditable="true"></pre></div></div></div><div class="modal-right"><div class="form-row form-block stretch"><label style="width:auto;">返回结果</label><div class="codebox"><span id="invoke_result_copy" class="copybtn">复制</span><div id="invoke_result" class="rpc-meta jsonviewer"></div></div></div></div></div><div class="modal-actions"><button class="btn" onclick="closeInvoke()">取消</button><button class="btn primary" onclick="submitInvoke()">确认发送</button></div><div class="toast">复制成功</div></div></div>
</body>
</html>`

type BranchInfo struct {
	Name string `json:"name"`
	Date string `json:"date"`
}

func getDocBranches(repoDir string) ([]BranchInfo, error) {
	// Try fetching latest refs first to ensure we have up-to-date info
	// Ignore errors here (e.g. no network), just try to use what we have
	_ = exec.Command("git", "-C", repoDir, "fetch", "--prune", "origin").Run()

	// Use user-provided format for reliability
	out, err := runGit(repoDir, "for-each-ref", "--sort=-committerdate", "--format=%(committerdate:format:%Y-%m-%d %H:%M:%S)|%(refname:short)", "refs/remotes/")
	if err != nil {
		return nil, err
	}
	// fmt.Println("DEBUG: branches out:\n", out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var branches []BranchInfo
	var activeBranches []BranchInfo

	seen := map[string]bool{}
	now := time.Now()

	for _, line := range lines {
		parts := strings.Split(line, "|")
		if len(parts) < 2 {
			continue
		}
		dateStr := strings.TrimSpace(parts[0])
		refShort := strings.TrimSpace(parts[1]) // origin/master or origin/feature/xyz

		// We only want 'origin' branches for now to avoid confusion
		if !strings.HasPrefix(refShort, "origin/") {
			continue
		}
		name := strings.TrimPrefix(refShort, "origin/")

		if name == "HEAD" || name == "" {
			continue
		}

		info := BranchInfo{Name: name, Date: dateStr}

		// Always include master/main
		if name == "master" || name == "main" {
			if !seen[name] {
				branches = append(branches, info)
				seen[name] = true
			}
			continue
		}

		// Check time (30 days)
		t, err := time.Parse("2006-01-02 15:04:05", dateStr)
		if err == nil && now.Sub(t) <= 30*24*time.Hour {
			if !seen[name] {
				activeBranches = append(activeBranches, info)
				seen[name] = true
			}
		}
	}

	// Limit active branches to 10
	if len(activeBranches) > MaxBranches {
		activeBranches = activeBranches[:MaxBranches]
	}

	// Combine
	for _, b := range activeBranches {
		branches = append(branches, b)
	}
	if len(branches) == 0 {
		branches = append(branches, BranchInfo{Name: "master", Date: ""})
	}

	return branches, nil
}

func setupWorktree(repoDir, branch string) (string, func(), error) {
	sum := md5.Sum([]byte(repoDir + branch))
	hash := hex.EncodeToString(sum[:])
	tmpDir := filepath.Join(os.TempDir(), "doc_gen_"+hash)

	// Remove if exists (cleanup previous run)
	_ = os.RemoveAll(tmpDir)

	// git worktree add --detach -f <tmpDir> origin/<branch>
	_, err := runGit(repoDir, "worktree", "add", "--detach", "-f", tmpDir, "origin/"+branch)
	if err != nil {
		return "", nil, err
	}

	cleanup := func() {
		runGit(repoDir, "worktree", "remove", "--force", tmpDir)
		// Also remove dir just in case
		os.RemoveAll(tmpDir)
	}
	return tmpDir, cleanup, nil
}

func writeProjectFiles(docDir, groupKey, projectName, branchName string, out ProjectDoc) (string, string, error) {
	// Count slashes in branchName to determine depth
	depth := strings.Count(branchName, "/") + 1
	pathPrefix := strings.Repeat("../", depth)
	// Force hardcoded value as requested by user
	groupIndex := "../../index.html"

	proj := filepath.Join(docDir, groupKey, projectName, branchName)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		return "", "", err
	}
	docName := projectName + "_doc.json"
	idxName := "index.html"
	jb, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(filepath.Join(proj, docName), jb, 0o644); err != nil {
		return "", "", err
	}
	page := strings.ReplaceAll(fullPageTemplate, "__DOC_JSON__", docName)
	page = strings.ReplaceAll(page, "__CONSUL_ADDR__", ConsulAddr)

	injection := fmt.Sprintf("<script>window.CURRENT_BRANCH = '%s'; window.PATH_PREFIX = '%s'; window.GROUP_INDEX = '%s';</script>\n<script>", branchName, pathPrefix, groupIndex)
	page = strings.Replace(page, "<script>", injection, 1)

	if err := os.WriteFile(filepath.Join(proj, idxName), []byte(page), 0o644); err != nil {
		return "", "", err
	}
	return docName, idxName, nil
}

func buildProject(root, projectName string) *ProjectDoc {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("[ERROR] buildProject panic:", root, r)
		}
	}()
	apiDir := filepath.Join(root, "api")
	var protoCandidates []string
	filepath.WalkDir(apiDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".proto") {
			protoCandidates = append(protoCandidates, path)
		}
		return nil
	})
	if len(protoCandidates) == 0 {
		return nil
	}
	protoPath := protoCandidates[0]
	protoText := readFile(protoPath)
	methodsRaw, _, serviceName := parseProtoWithCategories(protoText)
	goFiles := listGoFiles(root)
	goSources := map[string]string{}
	for _, p := range goFiles {
		goSources[p] = readFile(p)
	}
	structs, aliases := parseStructs(goSources)
	serviceFull := serviceName + "." + serviceName
	out := ProjectDoc{Service: serviceFull, Proto: strings.TrimPrefix(protoPath, root+string(os.PathSeparator)), ApiDir: apiDir, Methods: []MethodDoc{}}
	for i, m := range methodsRaw {
		reqType, handler := findHandlerReqType(m["name"], goSources)
		givenRespType, givenRespSchema, givenRespFields, _ := findResponseInfo(m["name"], goSources, structs, aliases)
		respType := givenRespType
		if respType == "" && reqType != "" {
			respType = guessRespType(reqType, structs, goSources)
		}
		reqSchema := map[string]interface{}{}
		if reqType != "" {
			reqSchema = buildSchema(reqType, structs, aliases)
		}
		var respSchema map[string]interface{}
		if givenRespSchema != nil {
			respSchema = givenRespSchema
		} else if respType != "" {
			respSchema = buildSchema(respType, structs, aliases)
		} else {
			respSchema = map[string]interface{}{}
		}
		reqFields := []Field{}
		if reqType != "" {
			reqFields = buildFieldList(reqType, structs, aliases)
		}
		var respFields []map[string]string
		if givenRespFields != nil {
			respFields = givenRespFields
		} else if respType != "" {
			respFields = flattenResponseFields(respType, structs, aliases, "", nil)
		} else {
			respFields = []map[string]string{}
		}
		cat := m["category"]
		if cat == "" {
			cat = "未分类"
		}
		disp := m["display_name"]
		if disp == "" {
			disp = m["name"]
		}
		// git authors by proto line
		ln := 0
		if v := strings.TrimSpace(m["line"]); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				ln = n
			}
		}
		created := gitFirstAuthor(root, protoPath, ln)
		createdAt := gitFirstTime(root, protoPath, ln)
		last := gitLastAuthor(root, protoPath, ln)
		lastAt := gitLastTime(root, protoPath, ln)
		md := MethodDoc{Seq: i + 1, Name: m["name"], Comment: m["display_name"], Display: disp, GRPCPath: projectName + "/" + m["name"], Handler: handler, Method: "gRPC（Unary）", Category: cat, ReqType: reqType, RespType: respType, GitCreatedBy: created, GitLastModBy: last, GitCreatedAt: createdAt, GitLastModAt: lastAt, Request: reqSchema, RequestFld: reqFields, ResponseFld: respFields, RespEnv: map[string]interface{}{"code": 0, "message": "OK", "data": respSchema}, ProtoLine: ln}
		out.Methods = append(out.Methods, md)
	}
	catCount := map[string]int{}
	for _, mm := range out.Methods {
		catCount[mm.Category] = catCount[mm.Category] + 1
	}
	seenCat := map[string]bool{}
	ordered := []string{}
	for _, mm := range out.Methods {
		if !seenCat[mm.Category] {
			ordered = append(ordered, mm.Category)
			seenCat[mm.Category] = true
		}
	}
	// move 未分类 to the end
	hasUncat := false
	var orderedFixed []string
	for _, c := range ordered {
		if c == "未分类" {
			hasUncat = true
			continue
		}
		orderedFixed = append(orderedFixed, c)
	}
	if hasUncat {
		orderedFixed = append(orderedFixed, "未分类")
	}
	ordered = orderedFixed
	byCat := map[string][]MethodDoc{}
	for _, m := range out.Methods {
		byCat[m.Category] = append(byCat[m.Category], m)
	}
	var newMethods []MethodDoc
	for _, cat := range ordered {
		newMethods = append(newMethods, byCat[cat]...)
	}
	for idx := range newMethods {
		newMethods[idx].Seq = idx + 1
	}
	out.Methods = newMethods
	cats := []map[string]interface{}{}
	for _, name := range ordered {
		cats = append(cats, map[string]interface{}{"name": name, "count": catCount[name]})
	}
	// developer stats: created count and updated count (when created time != last updated time)
	devCreated := map[string]int{}
	devUpdated := map[string]int{}
	for _, mm := range out.Methods {
		if strings.TrimSpace(mm.GitCreatedBy) != "" {
			k := strings.TrimSpace(mm.GitCreatedBy)
			devCreated[k] = devCreated[k] + 1
		}
		cT, cOk := parseTimeFlexible(mm.GitCreatedAt)
		lT, lOk := parseTimeFlexible(mm.GitLastModAt)
		if strings.TrimSpace(mm.GitLastModBy) != "" && cOk && lOk {
			if !(cT.Year() == lT.Year() && cT.Month() == lT.Month() && cT.Day() == lT.Day()) {
				k := strings.TrimSpace(mm.GitLastModBy)
				devUpdated[k] = devUpdated[k] + 1
			}
		}
	}
	names := map[string]bool{}
	for n := range devCreated {
		names[n] = true
	}
	for n := range devUpdated {
		names[n] = true
	}
	devs := []map[string]interface{}{}
	for n := range names {
		devs = append(devs, map[string]interface{}{"name": n, "created": devCreated[n], "updated": devUpdated[n]})
	}
	sort.Slice(devs, func(i, j int) bool {
		ci := devs[i]["created"].(int)
		cj := devs[j]["created"].(int)
		if ci != cj {
			return ci > cj
		}
		ui := devs[i]["updated"].(int)
		uj := devs[j]["updated"].(int)
		if ui != uj {
			return ui > uj
		}
		return strings.ToLower(devs[i]["name"].(string)) < strings.ToLower(devs[j]["name"].(string))
	})
	out.Stats = map[string]interface{}{"total": len(out.Methods), "categories": cats, "category_total": len(cats), "developers": devs, "developer_total": len(devs), "envs": Envs}
	return &out
}

func deriveGroupKey(root string) string {
	parts := strings.Split(filepath.Clean(root), string(filepath.Separator))
	if len(parts) >= 2 && strings.EqualFold(parts[len(parts)-1], "services") {
		return parts[len(parts)-2] + "_services"
	}
	return filepath.Base(root) + "_services"
}
