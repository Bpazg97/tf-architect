package llm

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"tf-architect/internal/security"
)

// Config selects and configures a provider. API keys are read only from the
// environment (via Getenv), never from flags.
type Config struct {
	Provider  string
	Model     string
	BaseURL   string // overrides the provider's default; required for openai-compatible
	Getenv    func(string) string
	ClaudeBin string
}

// New builds the provider named by cfg.Provider.
func New(cfg Config) (Provider, error) {
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base != "" {
		if _, err := CheckBaseURL(base); err != nil {
			return nil, err
		}
	}
	key := func(name string) (string, error) {
		k := getenv(name)
		if k == "" {
			return "", fmt.Errorf("%s is not set (API keys are read from the environment only)", name)
		}
		return k, nil
	}
	needModel := func() error {
		if cfg.Model == "" {
			return fmt.Errorf("--model is required for %s", cfg.Provider)
		}
		return nil
	}
	orDefault := func(def string) string {
		if base != "" {
			return base
		}
		return def
	}

	switch cfg.Provider {
	case "claude":
		return &ClaudeCLI{Bin: cfg.ClaudeBin, Model: cfg.Model}, nil
	case "anthropic":
		k, err := key("ANTHROPIC_API_KEY")
		if err != nil {
			return nil, err
		}
		model := cfg.Model
		if model == "" {
			model = DefaultAnthropicModel
		}
		return &anthropic{key: k, model: model, baseURL: orDefault("https://api.anthropic.com/v1")}, nil
	case "openai":
		k, err := key("OPENAI_API_KEY")
		if err != nil {
			return nil, err
		}
		if err := needModel(); err != nil {
			return nil, err
		}
		return &openAI{name: "openai", key: k, model: cfg.Model, baseURL: orDefault("https://api.openai.com/v1")}, nil
	case "gemini":
		k, err := key("GEMINI_API_KEY")
		if err != nil {
			return nil, err
		}
		if err := needModel(); err != nil {
			return nil, err
		}
		return &gemini{key: k, model: cfg.Model, baseURL: orDefault("https://generativelanguage.googleapis.com/v1beta")}, nil
	case "openai-compatible":
		if base == "" {
			return nil, fmt.Errorf("--base-url is required for openai-compatible")
		}
		k, err := key("TFA_API_KEY")
		if err != nil {
			return nil, err
		}
		if err := needModel(); err != nil {
			return nil, err
		}
		return &openAI{name: "openai-compatible", key: k, model: cfg.Model, baseURL: base}, nil
	}
	return nil, fmt.Errorf("unknown provider %q (want claude, anthropic, openai, gemini or openai-compatible)", cfg.Provider)
}

// CheckBaseURL is IsLocalURL for a user-supplied --base-url: a URL that would
// carry prompts and the API key unencrypted (or that is not a URL at all) is
// a *security.Error.
func CheckBaseURL(raw string) (local bool, err error) {
	local, err = IsLocalURL(raw)
	if err != nil {
		return false, security.New(
			fmt.Sprintf("--base-url is not allowed: %v.", err),
			"Prompts and your API key would travel unencrypted or to an endpoint tf-architect cannot secure.",
			"Use an https:// URL (http:// only for localhost, 127.0.0.1 or [::1]).",
		)
	}
	return local, nil
}

// IsLocalURL reports whether raw points at this machine (localhost, 127.0.0.1
// or [::1]). It errors on anything that is not HTTPS or plain-HTTP loopback,
// so a key is never sent in clear text over a network.
func IsLocalURL(raw string) (bool, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return false, fmt.Errorf("invalid URL %q", raw)
	}
	h := u.Hostname()
	local := h == "localhost" || h == "127.0.0.1" || h == "::1"
	switch u.Scheme {
	case "https":
		return local, nil
	case "http":
		if local {
			return true, nil
		}
		return false, fmt.Errorf("refusing non-HTTPS URL for non-local host %q", h)
	}
	return false, fmt.Errorf("unsupported scheme %q in %q (use https)", u.Scheme, raw)
}
