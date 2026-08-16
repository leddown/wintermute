package twire

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"wintermute/internal/tool"
)

// Register exposes twire to the assistant, read-only.
//
// Only read access is offered, and that is a deliberate limit rather than an
// unfinished one: enabling a canary opens a listening socket on the host, and
// the alert configuration holds an SMTP credential. Both belong to a person
// driving the UI, not to a model choosing arguments — the same line the fintech
// package draws around CSV import and Kraken sync.
//
// Morpheus passed a user id into every tool handler and twire ignored it, being
// a global resource there too. Here there is no id to ignore: the bearer token
// at the API edge is the boundary.
func Register(reg *tool.Registry, svc *Service) error {
	for _, r := range []registration{
		canaryStatusTool(svc),
		canaryEventsTool(svc),
	} {
		if err := reg.Register(r.def, r.handler); err != nil {
			return err
		}
	}
	return nil
}

// registration pairs a tool with the handler that runs it.
type registration struct {
	def     tool.Definition
	handler tool.Handler
}

// jsonHandler adapts a handler that returns a value to the registry's, which
// returns the string the model sees.
func jsonHandler(fn func(ctx context.Context, raw json.RawMessage) (any, error)) tool.Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		v, err := fn(ctx, raw)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("encode tool result: %w", err)
		}
		return string(data), nil
	}
}

func canaryStatusTool(svc *Service) registration {
	return registration{
		def: tool.Definition{
			Name: "list_canary_status",
			Risk: tool.RiskRead,
			Description: "List the twire honeypot canaries (fake services on well-known ports) with whether each is enabled, " +
				"currently listening, and how many connection attempts it has recorded.",
			Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
		},
		handler: jsonHandler(func(ctx context.Context, _ json.RawMessage) (any, error) {
			canaries, err := svc.Status(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, len(canaries))
			for i, c := range canaries {
				out[i] = map[string]any{
					"service":   c.Name,
					"port":      c.Port,
					"enabled":   c.Enabled,
					"listening": c.Listening,
					"hit_count": c.HitCount,
					"error":     c.LastError,
				}
			}
			return out, nil
		}),
	}
}

func canaryEventsTool(svc *Service) registration {
	type input struct {
		Limit int `json:"limit"`
	}
	return registration{
		def: tool.Definition{
			Name: "list_canary_events",
			Risk: tool.RiskRead,
			Description: "List the most recent connection attempts recorded by the twire honeypot canaries (each is a potential " +
				"network probe). Returns the service, source IP, and time.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"limit": {"type": "integer", "description": "Maximum number of events to return. Omit for a sane default."}
				}
			}`),
		},
		handler: jsonHandler(func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in input
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &in)
			}
			events, err := svc.ListEvents(ctx, in.Limit)
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, len(events))
			for i, e := range events {
				out[i] = map[string]any{
					"service":      e.ServiceName,
					"port":         e.Port,
					"source_ip":    e.RemoteIP,
					"data_preview": e.DataPreview,
					"occurred_at":  e.OccurredAt.Format(time.RFC3339),
				}
			}
			return out, nil
		}),
	}
}
