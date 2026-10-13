package mcp

import "encoding/json"

// EnvToken is the environment variable the bridge reads its engine
// token from (§12.2): never a flag — flags are public to anyone listing
// processes — and never the harness's own environment: the harness's
// MCP config names it in the server's env, which the harness passes to
// the bridge process alone.
const EnvToken = "BRIGADA_TOKEN"

// harnessConfig is the harness's MCP config file as a foreman's harness
// reads it (§12.2): one server, the bridge itself.
type harnessConfig struct {
	Servers map[string]harnessServer `json:"mcpServers"`
}

// harnessServer is the one server the config offers: this binary, run
// with the mcp subcommand over the engine's socket, with the turn's
// token in its environment.
type harnessServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// HarnessConfig renders the harness's MCP config for one foreman turn
// (§12.2): one server, `brigada` — the command the runner names (its own
// binary, os.Executable) run as `mcp --socket <socket>`, with the
// lease-scoped foreman token in the environment under EnvToken. The
// runner writes the bytes to a private file and deletes it when the
// turn ends; the token reaches the file and the bridge alone.
func HarnessConfig(command, socket, token string) ([]byte, error) {
	cfg := harnessConfig{
		Servers: map[string]harnessServer{
			ServerName: {
				Command: command,
				Args:    []string{"mcp", "--socket", socket},
				Env:     map[string]string{EnvToken: token},
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return data, nil
}
