// Copyright 2026 Zibby Lab. Apache-2.0.

package mcp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ZibbyDev/agent-ops/internal/integrate"
)

// toolIntegrateAdd wires `agent-ops integrate add` semantics over MCP.
// Args mirror the CLI flags but as JSON. On success the response
// includes restart_required:true so a remote agent (Claude Code etc) can
// surface the hint to the user.
//
// We require ConfigPath on the Server — if the daemon was constructed in
// a test without one, the tool returns an isError result rather than
// silently rewriting a default path the operator might not even use.
func (s *Server) toolIntegrateAdd(w http.ResponseWriter, id any, raw json.RawMessage) {
	var args struct {
		Name      string            `json:"name"`
		Transport string            `json:"transport"`
		URL       string            `json:"url"`
		Command   string            `json:"command"`
		Args      []string          `json:"args"`
		AuthEnv   string            `json:"auth_env"`
		Token     string            `json:"token"`
		ExtraEnv  map[string]string `json:"extra_env"`
		StdioEnv  map[string]string `json:"stdio_env"`
		EnvFile   string            `json:"env_file"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if s.configPath == "" {
		s.respond(w, id, toolErrorResult(
			"daemon was constructed without a config path; use the CLI `agent-ops integrate add` instead"))
		return
	}
	res, err := integrate.Add(integrate.AddSpec{
		Name:      args.Name,
		Transport: args.Transport,
		URL:       args.URL,
		Command:   args.Command,
		Args:      args.Args,
		AuthEnv:   args.AuthEnv,
		Token:     args.Token,
		ExtraEnv:  args.ExtraEnv,
		StdioEnv:  args.StdioEnv,
	}, integrate.Options{
		ConfigPath: s.configPath,
		EnvPath:    args.EnvFile,
	})
	if err != nil {
		if errors.Is(err, integrate.ErrAlreadyExists) {
			s.respond(w, id, toolErrorResult(err.Error()))
			return
		}
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.log.Info("mcp: integrate add",
		"name", res.Name, "config_path", res.ConfigPath, "env_path", res.EnvPath)
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{
		"ok":               true,
		"name":             res.Name,
		"restart_required": res.RestartRequired,
		"config_path":      res.ConfigPath,
		"env_path":         res.EnvPath,
		"next_step":        "run `agent-ops restart` for the daemon to pick up the new integration",
	})))
}

func (s *Server) toolIntegrateRemove(w http.ResponseWriter, id any, raw json.RawMessage) {
	var args struct {
		Name    string `json:"name"`
		EnvFile string `json:"env_file"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		writeJSONRPCError(w, id, -32602, "bad args: "+err.Error())
		return
	}
	if s.configPath == "" {
		s.respond(w, id, toolErrorResult(
			"daemon was constructed without a config path; use the CLI `agent-ops integrate remove` instead"))
		return
	}
	res, err := integrate.Remove(args.Name, integrate.Options{
		ConfigPath: s.configPath,
		EnvPath:    args.EnvFile,
	})
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.log.Info("mcp: integrate remove", "name", res.Name)
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{
		"ok":               true,
		"name":             res.Name,
		"restart_required": res.RestartRequired,
		"removed_auth_env": res.RemovedAuthEnv,
		"next_step":        "run `agent-ops restart` to apply",
	})))
}

func (s *Server) toolIntegrateList(w http.ResponseWriter, id any) {
	if s.configPath == "" {
		s.respond(w, id, toolErrorResult(
			"daemon was constructed without a config path; use the CLI `agent-ops integrate list` instead"))
		return
	}
	items, err := integrate.List(integrate.Options{ConfigPath: s.configPath})
	if err != nil {
		s.respond(w, id, toolErrorResult(err.Error()))
		return
	}
	s.respond(w, id, toolTextResult(mustEncodeJSON(map[string]any{
		"integrations": items,
		"count":        len(items),
	})))
}
