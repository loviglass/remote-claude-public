package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Project files stay on the workstation. Only executable configuration entries
// are rendered as transport adapters; all other settings retain their native
// structure, precedence and tool names. Writes unwrap the adapters again.
func projectedConfig(path string) bool {
	p := strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "/")
	if strings.HasSuffix(p, ".md") && (strings.HasPrefix(p, ".claude/") || strings.Contains(p, "/.claude/") || strings.Contains(p, "/skills/") || strings.Contains(p, "/agents/")) {
		return true
	}
	return p == ".mcp.json" || strings.HasSuffix(p, "/.mcp.json") || p == ".claude/settings.json" || p == ".claude/settings.local.json" || strings.HasSuffix(p, "/.claude/settings.json") || strings.HasSuffix(p, "/.claude/settings.local.json") || strings.HasSuffix(p, "/hooks.json") || strings.HasSuffix(p, "/.claude-plugin/plugin.json")
}

type mcpProjection func(map[string]any, bool) (map[string]any, bool, error)

func projectJSON(data []byte, unwrap bool, projectors ...mcpProjection) ([]byte, error) {
	// Skills and agents support hook configuration in YAML frontmatter.
	if bytes.HasPrefix(data, []byte("---\n")) || bytes.HasPrefix(data, []byte("---\r\n")) {
		lf := bytes.IndexByte(data, '\n') + 1
		end := bytes.Index(data[lf:], []byte("\n---"))
		if end < 0 {
			return data, nil
		}
		end += lf
		var front map[string]any
		if yaml.Unmarshal(data[lf:end], &front) != nil {
			return data, nil
		}
		raw, e := json.Marshal(front)
		if e != nil {
			return data, nil
		}
		projected, e := projectJSON(raw, unwrap, projectors...)
		if e != nil || bytes.Equal(projected, raw) {
			return data, e
		}
		if e = json.Unmarshal(projected, &front); e != nil {
			return nil, e
		}
		y, e := yaml.Marshal(front)
		if e != nil {
			return nil, e
		}
		return append(append([]byte("---\n"), y...), data[end+1:]...), nil
	}
	var root any
	if e := json.Unmarshal(data, &root); e != nil {
		return data, nil
	} // Claude reports invalid source JSON itself.
	changed := false
	var projectionError error
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if hooks, ok := x["hooks"].(map[string]any); ok {
				for _, entries := range hooks {
					a, _ := entries.([]any)
					for _, entry := range a {
						m, _ := entry.(map[string]any)
						list, _ := m["hooks"].([]any)
						for i, item := range list {
							h, ok := item.(map[string]any)
							if !ok {
								continue
							}
							if unwrap {
								if o, ok := unwrapDispatch(h, "hook"); ok {
									list[i] = o
									changed = true
								}
								continue
							}
							typ, _ := h["type"].(string)
							if typ == "command" || typ == "http" {
								q := map[string]any{}
								for k, v := range h {
									if k != "command" && k != "args" && k != "shell" && k != "url" && k != "headers" && k != "allowedEnvVars" {
										q[k] = v
									}
								}
								q["type"] = "command"
								q["command"] = serverBinary
								q["args"] = []any{"dispatch", "hook", encode(h)}
								list[i] = q
								changed = true
							}
						}
					}
				}
			}
			if servers, ok := x["mcpServers"].(map[string]any); ok {
				for name, item := range servers {
					m, ok := item.(map[string]any)
					if !ok {
						continue
					}
					if len(projectors) > 0 && projectors[0] != nil {
						if v, done, e := projectors[0](m, unwrap); e != nil {
							projectionError = e
							return
						} else if done {
							servers[name] = v
							changed = true
							continue
						}
					}
					if unwrap {
						if o, ok := unwrapDispatch(m, "mcp"); ok {
							servers[name] = o
							changed = true
						}
						continue
					}
					if command, ok := m["command"].(string); ok && command != "" {
						q := map[string]any{}
						for k, v := range m {
							if k != "command" && k != "args" && k != "env" && k != "cwd" {
								q[k] = v
							}
						}
						q["command"] = serverBinary
						q["args"] = []any{"dispatch", "mcp", encode(m)}
						servers[name] = q
						changed = true
					}
				}
			}
			// Recurse into configuration objects, not freshly encoded transport specs.
			for k, y := range x {
				if k != "hooks" && k != "mcpServers" {
					walk(y)
				}
			}
		case []any:
			for _, y := range x {
				walk(y)
			}
		}
	}
	walk(root)
	if projectionError != nil {
		return nil, projectionError
	}
	if !changed {
		return data, nil
	}
	b, e := json.MarshalIndent(root, "", "  ")
	return append(b, '\n'), e
}
func unwrapDispatch(m map[string]any, kind string) (map[string]any, bool) {
	if m["command"] != serverBinary {
		return nil, false
	}
	a, ok := m["args"].([]any)
	if !ok || len(a) != 3 || a[0] != "dispatch" || a[1] != kind {
		return nil, false
	}
	s, ok := a[2].(string)
	if !ok {
		return nil, false
	}
	var original map[string]any
	if decode(s, &original) != nil {
		return nil, false
	}
	return original, true
}
func stringArgs(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("args must be an array")
	}
	out := make([]string, len(a))
	for i, x := range a {
		var ok bool
		out[i], ok = x.(string)
		if !ok {
			return nil, fmt.Errorf("args[%d] must be a string", i)
		}
	}
	return out, nil
}
