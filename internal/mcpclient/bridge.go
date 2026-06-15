// Copyright 2026 Zibby Lab. Apache-2.0.

package mcpclient

import (
	"context"
	"encoding/json"

	"github.com/ZibbyDev/agent-ops/internal/tool"
)

// ToolInvoker adapts a mcpclient.Client into the tool.RemoteInvoker
// interface expected by tool.RemoteToolAdapter. Kept here (rather than in
// `internal/tool`) to preserve the dependency direction — tool has no
// awareness of mcpclient; mcpclient knows about tool.
//
// Each registered RemoteToolAdapter holds a pointer to one ToolInvoker;
// when the LLM calls the local tool name, the adapter passes the call
// through here and onto the wire.
type ToolInvoker struct {
	C Client
}

// Name returns the underlying client's name.
func (i *ToolInvoker) Name() string { return i.C.Name() }

// CallTool forwards to the underlying client and translates the
// mcpclient.CallResult into the tool-package's mirror type.
func (i *ToolInvoker) CallTool(ctx context.Context, name string, args json.RawMessage) (tool.RemoteCallResult, error) {
	res, err := i.C.CallTool(ctx, name, args)
	if err != nil {
		return tool.RemoteCallResult{}, err
	}
	return tool.RemoteCallResult{Text: res.Text, IsError: res.IsError}, nil
}
