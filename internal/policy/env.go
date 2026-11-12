package policy

import "strings"

// ProxyEnv are the variables that send an agent CLI's requests through
// another host: the facts a worker reports would then not say where the
// requests really go.
var ProxyEnv = map[string]bool{"HTTPS_PROXY": true, "HTTP_PROXY": true, "ALL_PROXY": true}

// EndpointEnv are the variables that change where an agent CLI sends its
// requests. An account's env may set them (the facts are derived from
// them); a stage's env may not, because facts are reported per runner, not
// per workspace and stage.
var EndpointEnv = map[string]bool{
	"ANTHROPIC_BASE_URL": true, "ANTHROPIC_BEDROCK_BASE_URL": true, "ANTHROPIC_VERTEX_BASE_URL": true, "ANTHROPIC_FOUNDRY_BASE_URL": true,
	"CLAUDE_CODE_USE_BEDROCK": true, "CLAUDE_CODE_USE_VERTEX": true, "CLAUDE_CODE_USE_FOUNDRY": true,
	"OPENAI_BASE_URL": true, "OPENCODE_CONFIG": true, "OPENCODE_CONFIG_CONTENT": true,
}

// RoutesRequests reports a variable that changes where an agent's requests
// go: a proxy or an endpoint variable, in any case.
func RoutesRequests(name string) bool {
	up := strings.ToUpper(strings.TrimSpace(name))
	return ProxyEnv[up] || EndpointEnv[up]
}
