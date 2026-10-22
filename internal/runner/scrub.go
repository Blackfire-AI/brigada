package runner

import (
	"os"
	"strings"
)

// credentialEnvNames are the ambient variables that carry the user's own
// credentials to the services their tools reach — git forges, cloud
// providers, package registries, an ssh agent. The runner holds them
// because the user's shell did; none of them belongs to a worker, a
// setup command or a check, which run code the task or the repository
// chose (§5.8). The vendors' own model logins (ANTHROPIC_*, OPENAI_*)
// are not here: the agent CLIs need those to run.
var credentialEnvNames = map[string]bool{
	// git and its forges
	"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true,
	"GITLAB_TOKEN": true, "GITLAB_PRIVATE_TOKEN": true, "GL_TOKEN": true, "CI_JOB_TOKEN": true,
	"BITBUCKET_TOKEN": true, "BITBUCKET_APP_PASSWORD": true,
	"GIT_ASKPASS": true, "SSH_ASKPASS": true, "SSH_AUTH_SOCK": true,
	"GIT_SSH": true, "GIT_SSH_COMMAND": true, "GIT_CONFIG_COUNT": true,
	// cloud providers
	"AWS_ACCESS_KEY_ID": true, "AWS_SECRET_ACCESS_KEY": true, "AWS_SESSION_TOKEN": true,
	"AWS_SECURITY_TOKEN": true, "AWS_PROFILE": true, "AWS_SHARED_CREDENTIALS_FILE": true,
	"AWS_WEB_IDENTITY_TOKEN_FILE": true, "AWS_CONTAINER_AUTHORIZATION_TOKEN": true,
	"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": true, "AWS_BEARER_TOKEN_BEDROCK": true,
	"GOOGLE_APPLICATION_CREDENTIALS": true, "GOOGLE_CREDENTIALS": true, "GOOGLE_OAUTH_ACCESS_TOKEN": true,
	"CLOUDSDK_AUTH_ACCESS_TOKEN": true, "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE": true,
	"DIGITALOCEAN_ACCESS_TOKEN": true, "HCLOUD_TOKEN": true, "CLOUDFLARE_API_TOKEN": true,
	"CF_API_TOKEN": true, "FLY_API_TOKEN": true, "VERCEL_TOKEN": true, "NETLIFY_AUTH_TOKEN": true,
	"HEROKU_API_KEY": true, "KUBECONFIG": true, "VAULT_TOKEN": true, "OP_SERVICE_ACCOUNT_TOKEN": true,
	// package registries and container registries
	"NPM_TOKEN": true, "NODE_AUTH_TOKEN": true, "PYPI_TOKEN": true, "TWINE_PASSWORD": true,
	"CARGO_REGISTRY_TOKEN": true, "GEM_HOST_API_KEY": true, "DOCKER_AUTH_CONFIG": true,
}

// credentialEnvPrefixes cover families of variables: a git config
// injected through the environment (it can carry an auth header), and
// the providers whose every variable is a login.
var credentialEnvPrefixes = []string{
	"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_", "AZURE_", "ARM_CLIENT_", "CARGO_REGISTRIES_",
}

// isCredentialEnv reports whether name is an ambient credential variable.
func isCredentialEnv(name string) bool {
	if credentialEnvNames[name] {
		return true
	}
	for _, p := range credentialEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// brigadaEnvPrefix is the namespace of brigada's own variables; the
// runner's process may hold some (a token, a socket path), and a command
// the repository chose has no business reading them.
const brigadaEnvPrefix = "BRIGADA_"

// scrubWorkerEnv is env — nil meaning the process's — without the runner's
// git credentials (gitEnv, §5.1) and without the ambient credentials of
// credentialEnvNames: what a worker CLI is started with (§5.8). The
// adapters already drop the brigada-prefixed variables themselves.
func scrubWorkerEnv(env, gitEnv []string) []string {
	return dropEnv(env, gitEnv, false)
}

// scrubCommandEnv is scrubWorkerEnv's env for a command the repository
// chose — a setup command or a check — which also loses every
// brigada-prefixed variable. The extra checkouts' BRIGADA_REPO_<NAME>
// and the cache directory are added back by the caller after the scrub.
func scrubCommandEnv(env, gitEnv []string) []string {
	return dropEnv(env, gitEnv, true)
}

func dropEnv(env, gitEnv []string, brigada bool) []string {
	if env == nil {
		env = os.Environ()
	}
	drop := make(map[string]bool, len(gitEnv))
	for _, kv := range gitEnv {
		if k, _, ok := strings.Cut(kv, "="); ok && k != "" {
			drop[k] = true
		}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if ok && (drop[k] || isCredentialEnv(k) || brigada && strings.HasPrefix(k, brigadaEnvPrefix)) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// commandEnv is the environment of a setup command or a check: the
// process's, scrubbed as scrubCommandEnv says, then the job's own
// variables (the extra checkouts' paths, the cache directory).
func (c *Config) commandEnv(extra ...string) []string {
	return append(scrubCommandEnv(nil, c.GitEnv), extra...)
}
