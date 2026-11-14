package policy

import (
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Who receives the code: a provider name derived from the endpoint a runner
// sends its requests to. A worker reports it with the runner's facts; a
// rule matches on it.

// ProviderLocal is a model on the worker's own machine (a loopback
// endpoint), or one the operator declared local.
const ProviderLocal = "local"

// UnknownPrefix marks a provider that could not be derived:
// "unknown:<host or id>". No rule matches it unless the rule names it, or
// names no provider at all.
const UnknownPrefix = "unknown:"

// hostProviders maps a domain suffix to the company behind it. Only
// domains the company itself serves from: a cloud's customer hosts
// (compute, API gateways, app hosting) are anyone's, so AWS and Azure are
// matched by their AI services' names below, not by the whole domain.
var hostProviders = []struct{ suffix, provider string }{
	{"anthropic.com", "anthropic"},
	{"claude.ai", "anthropic"},
	{"openai.com", "openai"},
	{"chatgpt.com", "openai"},
	{"z.ai", "zai"},
	{"bigmodel.cn", "zai"},
	{"googleapis.com", "google"},
	{"openai.azure.com", "microsoft"},
	{"services.ai.azure.com", "microsoft"},
	{"cognitiveservices.azure.com", "microsoft"},
	{"x.ai", "xai"},
	{"mistral.ai", "mistral"},
	{"deepseek.com", "deepseek"},
	{"groq.com", "groq"},
	{"openrouter.ai", "openrouter"},
	{"githubcopilot.com", "github"},
}

// awsService reports whether host is Amazon Bedrock itself
// (bedrock[-runtime|-agent|-agent-runtime][-fips].<region>.amazonaws.com),
// not a customer's host. A customer names their own load balancers and
// buckets: bedrock-gw-123.eu-west-1.elb.amazonaws.com is one of those, so
// the service's own labels are matched exactly and nothing may stand
// between the region and amazonaws.com.
func awsService(host string) bool {
	rest, ok := strings.CutSuffix(host, ".amazonaws.com")
	if !ok {
		return false
	}
	labels := strings.Split(rest, ".")
	if len(labels) < 1 || len(labels) > 2 {
		return false
	}
	if len(labels) == 2 && !reAWSRegion.MatchString(labels[1]) {
		return false
	}
	switch strings.TrimSuffix(labels[0], "-fips") {
	case "bedrock", "bedrock-runtime", "bedrock-agent", "bedrock-agent-runtime":
		return true
	}
	return false
}

// reAWSRegion: eu-west-1, us-gov-east-1, ap-southeast-2.
var reAWSRegion = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d+$`)

// Contradicts reports whether a declared provider is at odds with what the
// endpoint says: the endpoint identifies a provider (it is not unknown) and
// it is a different one. Declaring what cannot be derived is never a
// contradiction.
func Contradicts(declared, derived string) bool {
	declared, derived = strings.ToLower(strings.TrimSpace(declared)), strings.ToLower(strings.TrimSpace(derived))
	return declared != "" && derived != "" && !strings.HasPrefix(derived, UnknownPrefix) && declared != derived
}

// Host returns the host of an endpoint given as a URL or a bare host
// (lower-cased, without port); "" when there is none.
func Host(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// ProviderForEndpoint derives the provider from an endpoint: a known
// domain's company, "local" for loopback, else "unknown:<host>". A host on
// a private network is not assumed local: a router there may forward to
// anyone.
func ProviderForEndpoint(endpoint string) string {
	host := Host(endpoint)
	if host == "" {
		return UnknownPrefix + "none"
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return ProviderLocal
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return ProviderLocal
		}
		return UnknownPrefix + host
	}
	for _, hp := range hostProviders {
		if host == hp.suffix || strings.HasSuffix(host, "."+hp.suffix) {
			return hp.provider
		}
	}
	if awsService(host) {
		return "aws"
	}
	return UnknownPrefix + host
}

// opencodeProviders maps opencode's built-in provider ids (the first part
// of a model string, "zai-coding-plan/glm-5.3") to the company behind
// them. Ids whose endpoint the operator chooses (ollama, lmstudio, any
// custom one) are not here: they need a baseURL, or a declared provider.
var opencodeProviders = map[string]string{
	"anthropic":               "anthropic",
	"openai":                  "openai",
	"google":                  "google",
	"google-vertex":           "google",
	"google-vertex-anthropic": "google",
	"amazon-bedrock":          "aws",
	"azure":                   "microsoft",
	"zai":                     "zai",
	"zai-coding-plan":         "zai",
	"zhipuai":                 "zai",
	"zhipuai-coding-plan":     "zai",
	"xai":                     "xai",
	"mistral":                 "mistral",
	"deepseek":                "deepseek",
	"groq":                    "groq",
	"openrouter":              "openrouter",
	"github-copilot":          "github",
	"opencode":                "opencode",
}

// ProviderForOpencode derives the provider from an opencode model string
// and, when the operator's config sets one, that provider's baseURL.
func ProviderForOpencode(model, baseURL string) (provider, endpoint string) {
	id, _, _ := strings.Cut(strings.TrimSpace(model), "/")
	id = strings.ToLower(id)
	if strings.TrimSpace(baseURL) != "" {
		return ProviderForEndpoint(baseURL), Host(baseURL)
	}
	if p, ok := opencodeProviders[id]; ok {
		return p, "opencode:" + id
	}
	if id == "" {
		return UnknownPrefix + "none", ""
	}
	return UnknownPrefix + id, "opencode:" + id
}
