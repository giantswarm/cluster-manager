package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/giantswarm/mcp-oauth/providers/oidc"

	"github.com/giantswarm/cluster-manager/internal/identity"
	"github.com/giantswarm/cluster-manager/internal/tools"
)

// GitHubExchangeConfig identifies a confidential RFC 8693 client and its
// operator-controlled target. No caller can select the endpoint or target.
type GitHubExchangeConfig struct {
	TokenEndpoint string
	// ClientID wins over ClientIDFile.
	ClientID         string
	ClientIDFile     string
	ClientSecretFile string
	// Audience is the provider App whose grant the broker releases, also
	// named in the grant_required answer.
	Audience string
	CAFile   string
	APIURL   string
}

// Validate checks required fields.
func (c GitHubExchangeConfig) Validate() error {
	endpoint, err := url.Parse(c.TokenEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("GitHub token exchange requires an HTTPS token endpoint without credentials, query or fragment")
	}
	if (c.ClientID == "" && c.ClientIDFile == "") || c.ClientSecretFile == "" || c.Audience == "" {
		return errors.New("GitHub token exchange requires a client ID or client ID file, a client secret file and an audience")
	}
	if c.APIURL != "" {
		apiURL, err := url.Parse(c.APIURL)
		if err != nil || apiURL.Scheme != "https" || apiURL.Host == "" || apiURL.User != nil || apiURL.RawQuery != "" || apiURL.Fragment != "" {
			return errors.New("GitHub token exchange requires an HTTPS GitHub API URL")
		}
	}
	return nil
}

// resolveClientID returns ClientID, else the content of ClientIDFile.
func (c GitHubExchangeConfig) resolveClientID() (string, error) {
	if c.ClientID != "" {
		return c.ClientID, nil
	}
	content, err := os.ReadFile(c.ClientIDFile) // #nosec G304 -- operator-provided path
	if err != nil || strings.TrimSpace(string(content)) == "" {
		return "", fmt.Errorf("GitHub token exchange needs a client ID: --github-token-exchange-client-id is empty and %s holds none", c.ClientIDFile)
	}
	return strings.TrimSpace(string(content)), nil
}

// NewGitHubExchange obtains the person's user grant on demand. Tokens are
// neither cached nor attached to a session; only the returned request-local
// identity holds one. log receives the broker's refusals.
func NewGitHubExchange(configuration GitHubExchangeConfig, log *slog.Logger) (tools.GitHubUser, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	clientID, err := configuration.resolveClientID()
	if err != nil {
		return nil, err
	}
	configuration.ClientID = clientID
	roots, err := loadRootCAs(configuration.CAFile)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if roots != nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return newGitHubExchange(configuration, client, log), nil
}

// maxLoggedDescription bounds the broker's error_description in the log.
const maxLoggedDescription = 256

func newGitHubExchange(configuration GitHubExchangeConfig, client *http.Client, log *slog.Logger) tools.GitHubUser {
	// The endpoint is operator configuration, never a caller's, and an
	// in-cluster broker resolves to a private address: the client checks
	// only that it is HTTPS.
	exchanger := oidc.NewTokenExchangeClientWithOptions(oidc.TokenExchangeClientOptions{HTTPClient: client, Logger: slog.New(slog.DiscardHandler), AllowPrivateIP: true})
	grantRequired := func(reason string) error {
		return &tools.ErrGrantRequired{App: configuration.Audience, Reason: fmt.Sprintf(reason, configuration.Audience, configuration.Audience)}
	}
	return func(ctx context.Context) (*identity.GitHub, error) {
		_, ok := identity.FromContext(ctx)
		subjectToken, hasToken := identity.TokenFromContext(ctx)
		if !ok || !hasToken || subjectToken == "" {
			return nil, &tools.ErrRefused{Reason: "commit mode needs your verified platform identity to fetch your GitHub authorization"}
		}
		// A delegated caller (an agent acting for the person) never receives
		// the person's GitHub grant.
		if claims, err := oidc.ParseUnverifiedClaims(subjectToken); err != nil || claims["act"] != nil {
			return nil, &tools.ErrRefused{Reason: "forbidden: commit mode opens the pull request as you, and only you, not an agent acting for you, can authorize it"}
		}
		secret, err := os.ReadFile(configuration.ClientSecretFile)
		if err != nil || len(strings.TrimSpace(string(secret))) == 0 {
			return nil, errors.New("GitHub token exchange credentials are unavailable")
		}
		result, err := exchanger.Exchange(ctx, oidc.TokenExchangeRequest{
			TokenEndpoint:      configuration.TokenEndpoint,
			SubjectToken:       subjectToken,
			SubjectTokenType:   oidc.TokenTypeIDToken,
			RequestedTokenType: oidc.TokenTypeAccessToken,
			Audience:           configuration.Audience,
			ClientID:           configuration.ClientID,
			ClientSecret:       strings.TrimSpace(string(secret)),
		})
		if err != nil {
			refusal, ok := errors.AsType[*oidc.TokenExchangeError](err)
			if !ok {
				return nil, errors.New("GitHub authorization service is unavailable")
			}
			log.WarnContext(ctx, "GitHub token exchange refused", "status", refusal.StatusCode, "error", refusal.Code, "errorDescription", truncate(refusal.Description, maxLoggedDescription), "audience", configuration.Audience)
			// The broker answers invalid_target both for a person without the
			// grant and for an audience it does not serve; its description
			// does not tell them apart.
			if refusal.StatusCode == http.StatusBadRequest && refusal.Code == "invalid_target" {
				return nil, grantRequired("this call opens a pull request as you with your grant of the GitHub App %s in muster, which you have not given: grant it (core_auth_login app=%s), then call again")
			}
			return nil, errors.New("GitHub authorization service refused the exchange")
		}
		if !strings.EqualFold(result.TokenType, "Bearer") || result.IssuedTokenType != oidc.TokenTypeAccessToken || result.ExpiresIn <= 0 || result.RefreshToken != "" {
			return nil, errors.New("GitHub authorization service returned an invalid access token response")
		}
		apiURL := strings.TrimRight(configuration.APIURL, "/")
		if apiURL == "" {
			apiURL = DefaultGitHubAPIURL
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/user", nil)
		if err != nil {
			return nil, errors.New("GitHub API URL is invalid")
		}
		request.Header.Set("Authorization", "Bearer "+result.AccessToken)
		request.Header.Set("Accept", "application/vnd.github+json")
		response, err := client.Do(request)
		if err != nil {
			return nil, errors.New("GitHub user verification is unavailable")
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode == http.StatusUnauthorized {
			return nil, grantRequired("GitHub refused your grant of the GitHub App %s: grant it again in muster (core_auth_login app=%s), then call again")
		}
		var user struct {
			Login string `json:"login"`
		}
		if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 16*1024)).Decode(&user) != nil || user.Login == "" {
			return nil, fmt.Errorf("GitHub user verification is unavailable (status %d)", response.StatusCode)
		}
		return &identity.GitHub{Login: user.Login, Token: result.AccessToken}, nil
	}
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
