package peggy

import (
	"context"
	"fmt"
	"strings"

	"github.com/erain/glue"
	"github.com/erain/glue/daemon"
)

const (
	PermissionChannelCLI      = "cli"
	PermissionChannelTelegram = "telegram"
)

// PermissionTier names Peggy's product-level side-effect policy for a channel.
type PermissionTier string

const (
	PermissionTierPrompt   PermissionTier = "prompt"
	PermissionTierReadOnly PermissionTier = "read_only"
	PermissionTierTrusted  PermissionTier = "trusted"
)

// PermissionTierForChannel returns the configured tier for a channel name.
func PermissionTierForChannel(settings PermissionSettings, channel string) PermissionTier {
	settings = normalizePermissionSettings(settings)
	key := normalizePermissionChannel(channel)
	if key != "" && settings.Channels != nil {
		if raw := settings.Channels[key]; strings.TrimSpace(raw) != "" {
			return PermissionTier(normalizePermissionTier(raw))
		}
	}
	return PermissionTier(normalizePermissionTier(settings.DefaultTier))
}

// NewTieredPermission wraps a channel permission UI with Peggy's product-level
// permission tier. Prompt delegates to inner; read_only and trusted answer
// before any prompt UI is shown.
func NewTieredPermission(inner glue.Permission, tier PermissionTier, channel string) glue.Permission {
	return tieredPermission{
		inner:   inner,
		tier:    PermissionTier(normalizePermissionTier(string(tier))),
		channel: normalizePermissionChannel(channel),
	}
}

type tieredPermission struct {
	inner   glue.Permission
	tier    PermissionTier
	channel string
}

func (p tieredPermission) Decide(ctx context.Context, req glue.PermissionRequest) (glue.PermissionDecision, error) {
	switch p.tier {
	case "", PermissionTierPrompt:
		if p.inner == nil {
			return glue.PermissionDecision{Allow: false, Reason: "permission denied: no permission handler configured"}, nil
		}
		return p.inner.Decide(ctx, req)
	case PermissionTierReadOnly:
		return glue.PermissionDecision{Allow: false, Reason: permissionReadOnlyReason(p.channel)}, nil
	case PermissionTierTrusted:
		return glue.PermissionDecision{Allow: true}, nil
	default:
		return glue.PermissionDecision{}, fmt.Errorf("peggy: invalid permission tier %q", p.tier)
	}
}

// NewDaemonPermissionPolicy returns a daemon permission policy backed by Peggy
// settings. The daemon remains neutral; Peggy owns tier names and channel
// derivation.
func NewDaemonPermissionPolicy(settings PermissionSettings) daemon.PermissionPolicy {
	settings = normalizePermissionSettings(settings)
	return daemon.PermissionPolicyFunc(func(_ context.Context, info daemon.PermissionContext, req glue.PermissionRequest) (daemon.PermissionPolicyDecision, error) {
		channel := permissionChannelFromDaemon(info, req)
		tier := PermissionTierForChannel(settings, channel)
		switch tier {
		case PermissionTierPrompt:
			return daemon.PermissionPolicyDecision{Action: daemon.PermissionPolicyPrompt}, nil
		case PermissionTierReadOnly:
			return daemon.PermissionPolicyDecision{
				Action: daemon.PermissionPolicyDeny,
				Reason: permissionReadOnlyReason(channel),
			}, nil
		case PermissionTierTrusted:
			return daemon.PermissionPolicyDecision{Action: daemon.PermissionPolicyAllow}, nil
		default:
			return daemon.PermissionPolicyDecision{}, fmt.Errorf("peggy: invalid permission tier %q", tier)
		}
	})
}

func permissionChannelFromDaemon(info daemon.PermissionContext, req glue.PermissionRequest) string {
	if channel := channelPrefix(info.ClientID); channel != "" {
		return channel
	}
	if channel := channelPrefix(info.SessionID); channel != "" {
		return channel
	}
	return channelPrefix(req.SessionID)
}

func permissionReadOnlyReason(channel string) string {
	channel = normalizePermissionChannel(channel)
	if channel == "" {
		return "permission denied: channel is read-only"
	}
	return "permission denied: " + channel + " channel is read-only"
}

func normalizePermissionSettings(settings PermissionSettings) PermissionSettings {
	settings.DefaultTier = normalizePermissionTier(settings.DefaultTier)
	if settings.DefaultTier == "" {
		settings.DefaultTier = string(PermissionTierPrompt)
	}
	if len(settings.Channels) == 0 {
		return settings
	}
	channels := make(map[string]string, len(settings.Channels))
	for channel, tier := range settings.Channels {
		key := normalizePermissionChannel(channel)
		if key == "" {
			key = strings.TrimSpace(channel)
		}
		channels[key] = normalizePermissionTier(tier)
	}
	settings.Channels = channels
	return settings
}

func validatePermissionSettings(settings PermissionSettings) error {
	settings = normalizePermissionSettings(settings)
	if !validPermissionTier(settings.DefaultTier) {
		return fmt.Errorf("peggy: invalid permissions.default_tier %q", settings.DefaultTier)
	}
	for channel, tier := range settings.Channels {
		if !validPermissionTier(tier) {
			return fmt.Errorf("peggy: invalid permissions.channels.%s tier %q", channel, tier)
		}
	}
	return nil
}

func normalizePermissionTier(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func validPermissionTier(raw string) bool {
	switch PermissionTier(normalizePermissionTier(raw)) {
	case PermissionTierPrompt, PermissionTierReadOnly, PermissionTierTrusted:
		return true
	default:
		return false
	}
}

func normalizePermissionChannel(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func channelPrefix(raw string) string {
	raw = strings.TrimSpace(raw)
	prefix, _, ok := strings.Cut(raw, ":")
	if !ok || prefix == "" {
		return ""
	}
	return normalizePermissionChannel(prefix)
}
