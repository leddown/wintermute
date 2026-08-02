package lookup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"wintermute/internal/tool"
)

// lookupSchema is the JSON Schema shown to the model. It is written to steer a
// small model towards the right shape: kind is enumerated, and the season and
// episode conditions are stated in the descriptions because most local models
// ignore JSON Schema conditionals.
const lookupSchema = `{
  "type": "object",
  "properties": {
    "kind": {
      "type": "string",
      "enum": ["movie", "series", "episode"],
      "description": "What you are identifying. Use \"episode\" when you know the season and episode numbers."
    },
    "title": {
      "type": "string",
      "description": "The work's title, with release-group tags, resolution markers and separators stripped. For an episode, this is the series title, not the episode title."
    },
    "year": {
      "type": "integer",
      "description": "Release year, if the filename suggests one. Omit rather than guessing."
    },
    "season": {
      "type": "integer",
      "description": "Season number. Required when kind is \"episode\"."
    },
    "episode": {
      "type": "integer",
      "description": "Episode number within the season. Required when kind is \"episode\"."
    }
  },
  "required": ["kind", "title"]
}`

// Register adds the metadata lookup tool to reg, unless no provider is
// configured — in which case the model is never shown a tool it cannot use.
func Register(reg *tool.Registry, providers *Registry) error {
	if providers.Len() == 0 {
		return nil
	}
	def := tool.Definition{
		Name: "lookup_metadata",
		Description: fmt.Sprintf(
			"Look up a movie, TV series or TV episode in external metadata databases (%v) to confirm its canonical title, year and episode name. Use this before proposing a filename; do not rely on your own memory for titles or episode numbering.",
			providers.Names()),
		Parameters: json.RawMessage(lookupSchema),
		Risk:       tool.RiskRead,
	}
	return reg.Register(def, handler(providers))
}

func handler(providers *Registry) tool.Handler {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		var q Query
		if err := json.Unmarshal(input, &q); err != nil {
			return "", fmt.Errorf("invalid input: %w", err)
		}

		matches, providerErrs, err := providers.Search(ctx, q)
		if errors.Is(err, ErrNoProvider) {
			return fmt.Sprintf("No metadata provider is configured for kind %q.", q.Kind), nil
		}
		if err != nil {
			return "", err
		}

		// Both outcomes below are answers, not failures: the model needs to
		// see "nothing found" as a result it can act on by leaving the file be.
		out := struct {
			Query   Query    `json:"query"`
			Matches []Match  `json:"matches"`
			Note    string   `json:"note,omitempty"`
			Errors  []string `json:"errors,omitempty"`
		}{Query: q, Matches: matches}

		if len(matches) == 0 {
			out.Matches = []Match{}
			out.Note = "No matches. Do not invent a title; leave the file unchanged and say so."
		}
		for _, e := range providerErrs {
			out.Errors = append(out.Errors, e.Error())
		}

		buf, err := json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("encode result: %w", err)
		}
		return string(buf), nil
	}
}
