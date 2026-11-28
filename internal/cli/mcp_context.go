package cli

import (
	"crypto/sha256"
	"maps"
)

// mcpContextPin keeps command children on the startup selection, including when
// another process changes config between admission and authentication. OAuth
// refresh tokens may rotate, but the browser session and requested context stay
// fixed. API keys must continue to match the exact startup credential.
type mcpContextPin struct {
	opts          Options
	resolved      ResolvedConfig
	selection     mcpContextSelection
	credential    [32]byte
	apiKeyBaseURL string
	oauth         *oauthIdentity
	saved         bool
	err           error
}

type mcpContextSelection struct {
	profile, profileContext, context, merchantGuard, sandboxGuard string
	environment, apiKey, merchant, sandbox, store                 string
}

func mcpSelection(resolved ResolvedConfig, store string) mcpContextSelection {
	return mcpContextSelection{resolved.ProfileName, resolved.ProfileContextID, resolved.ContextID,
		resolved.MerchantGuard, resolved.SandboxGuard, resolved.Environment, resolved.APIKeyID,
		resolved.MerchantID, resolved.SandboxID, store}
}

func mcpContextChanged() *CLIError {
	return configError("MCP_CONTEXT_CHANGED", "The saved Flint context or credential changed after this MCP server started. No operation was run. Restart flint mcp serve to use the current context and credential.", nil)
}

func (a *App) pinMCPContext(opts Options) *mcpContextPin {
	pin := &mcpContextPin{opts: opts, saved: !environmentCredentialActive()}
	pin.err = a.withConfigLock(func() error {
		resolved, cfg, err := a.resolveConfig(opts)
		if err != nil {
			return err
		}
		pin.resolved = resolved
		pin.selection = mcpSelection(resolved, a.selectedCredentialStore(resolved.ProfileName, cfg.Profiles[resolved.ProfileName]))
		if !pin.saved {
			return nil // Environment credentials already belong to this process.
		}
		raw, err := a.loadSavedCredentialUnlocked(resolved.ProfileName)
		if err != nil {
			return err
		}
		pin.credential = sha256.Sum256([]byte(raw))
		pin.apiKeyBaseURL = cfg.Profiles[resolved.ProfileName].APIBaseURL
		if c, e := decodeOAuthCredential(raw); e == nil {
			pin.oauth = &oauthIdentity{Version: c.Version, BaseURL: c.BaseURL, SessionID: c.SessionID, GrantID: c.Auth.OAuthGrantID}
			if pin.resolved.ContextID == "" && c.Version == 2 {
				pin.resolved.ContextID = c.ContextID
			}
		}
		return nil
	})
	return pin
}

func (p *mcpContextPin) acceptsCredential(profile, raw string) bool {
	if profile != p.resolved.ProfileName {
		return false
	}
	if p.oauth == nil {
		return sha256.Sum256([]byte(raw)) == p.credential
	}
	c, e := decodeOAuthCredential(raw)
	return e == nil && c.Version == p.oauth.Version && c.BaseURL == p.oauth.BaseURL &&
		c.SessionID == p.oauth.SessionID && (c.Version == 2 || c.Auth.OAuthGrantID == p.oauth.GrantID)
}

func (a *App) checkMCPContext() *CLIError {
	pin := a.mcpContext
	if pin == nil {
		return nil
	}
	if pin.err != nil {
		return mcpContextChanged()
	}
	probe := *a
	probe.mcpContext = nil
	err := probe.withConfigLock(func() error {
		resolved, cfg, err := probe.resolveConfig(pin.opts)
		if err != nil {
			return err
		}
		store := probe.selectedCredentialStore(resolved.ProfileName, cfg.Profiles[resolved.ProfileName])
		if mcpSelection(resolved, store) != pin.selection {
			return mcpContextChanged()
		}
		if pin.saved {
			raw, err := probe.loadSavedCredentialUnlocked(resolved.ProfileName)
			if err != nil || !pin.acceptsCredential(resolved.ProfileName, raw) || (pin.oauth == nil && cfg.Profiles[resolved.ProfileName].APIBaseURL != pin.apiKeyBaseURL) {
				return mcpContextChanged()
			}
		}
		return nil
	})
	if err != nil {
		return mcpContextChanged()
	}
	return nil
}

func (p *mcpContextPin) resolve(a *App, opts Options) (ResolvedConfig, Config, error) {
	resolved := p.resolved
	resolved.Sources = maps.Clone(resolved.Sources)
	// Partner servers started without a merchant retain per-call selection.
	if resolved.MerchantGuard == "" && opts.Merchant != "" {
		resolved.MerchantGuard = opts.Merchant
		resolved.Sources["merchant_guard"] = "flag"
	}
	// Read mutable local state such as pending signup verification without
	// re-resolving the selection used for authentication.
	cfg, err := a.loadConfig()
	if p.err != nil {
		err = p.err
	}
	return resolved, cfg, err
}
