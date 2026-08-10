package models

import (
	"context"
	"encoding/json"
	"fmt"

	"wintermute/internal/tool"
)

// Register adds the model-awareness tools to reg.
//
// These let the assistant answer hardware and model questions in conversation
// — "what can this box run?", "which model should I use to draft a report?" —
// with the same numbers the Planner page shows, rather than from its training
// data. Every one of them is read-only: they inspect and estimate, and none
// changes what is running.
func Register(reg *tool.Registry, cat *Catalog) error {
	tools := []struct {
		def     tool.Definition
		handler tool.Handler
	}{
		{
			def: tool.Definition{
				Name:        "system_capabilities",
				Description: "Report the host's inference hardware: GPU model, total and free VRAM, compute capability, driver version, memory bandwidth, system RAM, and any neural accelerators. Call this before advising on which models can run, rather than assuming a typical machine.",
				Risk:        tool.RiskRead,
			},
			handler: systemCapabilities(cat),
		},
		{
			def: tool.Definition{
				Name:        "list_models",
				Description: "List the models currently available across all configured backends, with their size, quantization, capabilities, whether they are loaded, and an estimate of whether each fits in free VRAM.",
				Risk:        tool.RiskRead,
			},
			handler: listModels(cat),
		},
		{
			def: tool.Definition{
				Name:        "estimate_model_fit",
				Description: "Estimate the VRAM footprint and expected tokens per second of running a model of a given size and quantization at a given context length on this host. Use this to answer whether a specific model will run before recommending it.",
				Parameters:  json.RawMessage(fitSchema),
				Risk:        tool.RiskRead,
			},
			handler: estimateFit(cat),
		},
		{
			def: tool.Definition{
				Name:        "recommend_model",
				Description: "Rank models for a task against this host's actual hardware, returning a fit verdict, expected throughput and the command to serve each. Prefer this over recommending models from memory: it accounts for free VRAM and the requested context length.",
				Parameters:  json.RawMessage(recommendSchema),
				Risk:        tool.RiskRead,
			},
			handler: recommendModel(cat),
		},
		{
			def: tool.Definition{
				Name:        "search_models",
				Description: "Search the Hugging Face Hub for downloadable models, optionally restricted to GGUF repositories that llama.cpp and Ollama can load. Use this for models newer than the built-in catalog.",
				Parameters:  json.RawMessage(searchSchema),
				Risk:        tool.RiskRead,
			},
			handler: searchModels(cat),
		},
	}

	for _, t := range tools {
		if err := reg.Register(t.def, t.handler); err != nil {
			return err
		}
	}
	return nil
}

const fitSchema = `{
  "type": "object",
  "properties": {
    "params_b": {
      "type": "number",
      "description": "Parameter count in billions, e.g. 8 for an 8B model."
    },
    "quant": {
      "type": "string",
      "description": "Quantization label, e.g. Q4_K_M, Q5_K_M, Q8_0, F16. Defaults to Q4_K_M."
    },
    "context_tokens": {
      "type": "integer",
      "description": "Context window in tokens. Defaults to 8192. This competes with model weights for VRAM, so it materially changes the answer."
    },
    "kv_cache_type": {
      "type": "string",
      "enum": ["f16", "q8_0", "q4_0"],
      "description": "KV cache precision. Defaults to q8_0, which roughly halves the memory cost of context."
    },
    "active_params_b": {
      "type": "number",
      "description": "For mixture-of-experts models, the parameters active per token. Omit for dense models."
    }
  },
  "required": ["params_b"]
}`

const recommendSchema = `{
  "type": "object",
  "properties": {
    "task": {
      "type": "string",
      "enum": ["general", "agent", "documents", "coding", "long_context", "vision", "reasoning", "embedding"],
      "description": "The job the model is for. \"documents\" is long-form prose; \"agent\" is reliable tool calling."
    },
    "context_tokens": {
      "type": "integer",
      "description": "How much context the job needs, in tokens. Defaults to 8192."
    },
    "priority": {
      "type": "string",
      "enum": ["balanced", "quality", "speed"],
      "description": "What to optimise for when several models are viable. Defaults to balanced."
    },
    "require_tools": {
      "type": "boolean",
      "description": "Restrict to models that can emit tool calls."
    },
    "require_local": {
      "type": "boolean",
      "description": "Exclude cloud backends. Set this for anything sensitive."
    }
  },
  "required": ["task"]
}`

const searchSchema = `{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Search terms, e.g. a model family name."
    },
    "gguf_only": {
      "type": "boolean",
      "description": "Restrict to GGUF repositories, which are what llama.cpp and Ollama can load. Usually true."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum results, default 20."
    }
  },
  "required": ["query"]
}`

func systemCapabilities(cat *Catalog) tool.Handler {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		return encode(cat.Hardware(ctx))
	}
}

func listModels(cat *Catalog) tool.Handler {
	return func(ctx context.Context, _ json.RawMessage) (string, error) {
		list, err := cat.Models(ctx, 8192)
		if err != nil {
			return "", err
		}
		health, err := cat.BackendHealth(ctx)
		if err != nil {
			return "", err
		}
		if len(list) == 0 {
			return encode(map[string]any{
				"models":   []Model{},
				"backends": health,
				"note":     "No models found. A backend may be unreachable — check the backends list for health.",
			})
		}
		return encode(map[string]any{"models": list, "backends": health})
	}
}

func estimateFit(cat *Catalog) tool.Handler {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var in FitInput
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		if in.ParamsB <= 0 {
			return "params_b must be greater than zero.", nil
		}
		hw := cat.Hardware(ctx)
		fit := EstimateFit(in, hw)
		return encode(map[string]any{"input": in, "fit": fit, "hardware": hw})
	}
}

func recommendModel(cat *Catalog) tool.Handler {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var req PlanRequest
		if err := json.Unmarshal(input, &req); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		plan, err := cat.Recommend(ctx, req)
		if err != nil {
			return "", err
		}
		// Only the leading candidates are returned: the tail is noise in a
		// transcript, and the full list is on the Planner page.
		if len(plan.Recommendations) > 6 {
			plan.Recommendations = plan.Recommendations[:6]
		}
		return encode(plan)
	}
}

func searchModels(cat *Catalog) tool.Handler {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var in struct {
			Query    string `json:"query"`
			GGUFOnly bool   `json:"gguf_only"`
			Limit    int    `json:"limit"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}
		results, err := cat.Hub().Search(ctx, SearchOptions{
			Query:    in.Query,
			GGUFOnly: in.GGUFOnly,
			Limit:    in.Limit,
		})
		if err != nil {
			// A Hub outage is something the model should report, not a reason
			// to fail the turn.
			return fmt.Sprintf("Could not reach the Hugging Face Hub: %v", err), nil
		}
		if len(results) == 0 {
			return "No models matched that search.", nil
		}
		return encode(map[string]any{"results": results})
	}
}

func encode(v any) (string, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}
	return string(buf), nil
}
