package models

import (
	"fmt"
	"sort"
	"strings"
)

// Priority is what to optimise for when two models are both viable.
type Priority string

const (
	// PriorityBalanced weighs capability and speed evenly.
	PriorityBalanced Priority = "balanced"
	// PriorityQuality prefers the most capable model that runs at all, even
	// slowly.
	PriorityQuality Priority = "quality"
	// PrioritySpeed prefers the fastest model that is good enough.
	PrioritySpeed Priority = "speed"
)

// PlanRequest asks for a recommendation.
type PlanRequest struct {
	Task Task `json:"task"`
	// ContextTokens is how much context the job needs. This is a first-class
	// input, not a detail: on a memory-constrained card, context and model
	// size compete for the same VRAM, and asking for a long window can rule
	// out the model that would otherwise have won.
	ContextTokens int      `json:"context_tokens"`
	Priority      Priority `json:"priority"`
	// RequireTools restricts to models that can emit tool calls.
	RequireTools bool `json:"require_tools"`
	// RequireLocal excludes cloud backends. Set this for anything sensitive.
	RequireLocal bool `json:"require_local"`
}

// Recommendation is one ranked candidate.
type Recommendation struct {
	ID      string  `json:"id"`
	Display string  `json:"display"`
	Family  string  `json:"family"`
	ParamsB float64 `json:"params_b"`
	License string  `json:"license"`
	Quant   string  `json:"quant"`
	// Score is the final ranking score, 0-100.
	Score int `json:"score"`
	// TaskScore is the raw suitability for the task before hardware weighting.
	TaskScore int  `json:"task_score"`
	Fit       *Fit `json:"fit"`
	// Installed names the backend already serving this model, if any.
	Installed string `json:"installed,omitempty"`
	// Why explains the ranking in plain language.
	Why []string `json:"why"`
	// RunCommand is the exact command to serve it.
	RunCommand  string `json:"run_command,omitempty"`
	PullCommand string `json:"pull_command,omitempty"`
	Note        string `json:"note,omitempty"`
}

// Plan is the answer to a PlanRequest.
type Plan struct {
	Task            Task             `json:"task"`
	ContextTokens   int              `json:"context_tokens"`
	Priority        Priority         `json:"priority"`
	Hardware        *Hardware        `json:"hardware"`
	Recommendations []Recommendation `json:"recommendations"`
	// Summary is a one-paragraph answer for someone who does not want to read
	// a table.
	Summary string `json:"summary"`
}

// Recommend ranks the curated catalog for a task against real hardware.
//
// The ranking is deliberately hardware-first. A model that scores 95 on a task
// but has to spill half its layers to system RAM is worse, in practice, than
// one that scores 80 and runs entirely on the GPU — so the fit verdict
// multiplies the task score rather than merely annotating it.
func Recommend(req PlanRequest, hw *Hardware, installed []Model) *Plan {
	if req.Task == "" {
		req.Task = TaskGeneral
	}
	if req.ContextTokens <= 0 {
		req.ContextTokens = 8192
	}
	if req.Priority == "" {
		req.Priority = PriorityBalanced
	}

	installedBy := map[string]string{}
	for _, m := range installed {
		if seed, ok := matchSeed(m.ID); ok {
			installedBy[seed.ID] = m.Backend
		}
	}

	var recs []Recommendation
	for _, seed := range Seed {
		taskScore := seed.Scores[req.Task]
		if taskScore == 0 {
			continue
		}
		if req.RequireTools && !seed.Has(CapTools) {
			continue
		}

		quant := pickQuant(seed, req, hw)
		fit := EstimateFit(FitInput{
			ParamsB:       seed.ParamsB,
			ActiveParamsB: seed.ActiveB,
			Quant:         quant,
			ContextTokens: req.ContextTokens,
		}, hw)

		rec := Recommendation{
			ID:        seed.ID,
			Display:   seed.Display,
			Family:    seed.Family,
			ParamsB:   seed.ParamsB,
			License:   seed.License,
			Quant:     quant,
			TaskScore: taskScore,
			Fit:       &fit,
			Installed: installedBy[seed.ID],
			Note:      seed.Note,
		}
		rec.Score, rec.Why = score(seed, taskScore, fit, req)
		rec.RunCommand, rec.PullCommand = commands(seed, quant, req.ContextTokens)

		if seed.MaxCtx > 0 && req.ContextTokens > seed.MaxCtx {
			rec.Why = append(rec.Why, fmt.Sprintf(
				"Trained for %d tokens of context, less than the %d requested.", seed.MaxCtx, req.ContextTokens))
			rec.Score -= 20
		}
		if rec.Installed != "" {
			rec.Why = append(rec.Why, "Already available on the "+rec.Installed+" backend.")
			rec.Score += 5
		}
		if rec.Score < 0 {
			rec.Score = 0
		}
		if rec.Score > 100 {
			rec.Score = 100
		}
		recs = append(recs, rec)
	}

	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Score > recs[j].Score })

	plan := &Plan{
		Task:            req.Task,
		ContextTokens:   req.ContextTokens,
		Priority:        req.Priority,
		Hardware:        hw,
		Recommendations: recs,
	}
	plan.Summary = summarize(plan, req, hw)
	return plan
}

// pickQuant chooses a quantization for a candidate.
//
// The default is Q4_K_M everywhere. A small model on a card with room to spare
// is bumped up, because at that size the extra fidelity is close to free; a
// model that would otherwise not fit is stepped down before being written off.
func pickQuant(seed SeedModel, req PlanRequest, hw *Hardware) string {
	candidates := []string{"Q5_K_M", DefaultQuant, "IQ4_XS", "Q3_K_M"}
	if hw == nil || hw.PrimaryGPU() == nil {
		return DefaultQuant
	}
	for _, q := range candidates {
		fit := EstimateFit(FitInput{
			ParamsB:       seed.ParamsB,
			ActiveParamsB: seed.ActiveB,
			Quant:         q,
			ContextTokens: req.ContextTokens,
		}, hw)
		if fit.Verdict == VerdictFits {
			return q
		}
	}
	return DefaultQuant
}

// score combines task suitability with how well the model runs here.
func score(seed SeedModel, taskScore int, fit Fit, req PlanRequest) (int, []string) {
	var why []string
	value := float64(taskScore)

	switch fit.Verdict {
	case VerdictFits:
		why = append(why, fmt.Sprintf(
			"Runs entirely on the GPU: about %.1fGB of %.1fGB free VRAM, roughly %.0f tokens/sec.",
			fit.TotalMB/1024, fit.FreeVRAMMB/1024, fit.TokensPerSec))
	case VerdictTight:
		value *= 0.9
		why = append(why, fmt.Sprintf(
			"Fits, but only just: about %.1fGB against %.1fGB free.",
			fit.TotalMB/1024, fit.FreeVRAMMB/1024))
	case VerdictPartial:
		value *= 0.45
		why = append(why, fmt.Sprintf(
			"Does not fit — needs about %.1fGB against %.1fGB free, so roughly %d of %d layers spill to the CPU and throughput drops to around %.0f tokens/sec.",
			fit.TotalMB/1024, fit.FreeVRAMMB/1024, fit.TotalLayers-fit.GPULayers, fit.TotalLayers, fit.TokensPerSec))
	case VerdictNo:
		value *= 0.1
		why = append(why, "Will not run usefully on this hardware.")
	}

	switch req.Priority {
	case PrioritySpeed:
		// Reward throughput on a curve that flattens out: past about 40
		// tokens/sec a chat response already arrives faster than it can be
		// read, so more speed stops being worth quality.
		value *= 0.6 + 0.4*min1(fit.TokensPerSec/40)
		if fit.TokensPerSec >= 40 {
			why = append(why, "Fast enough to generate quicker than you can read.")
		}
	case PriorityQuality:
		// Reward parameter count, which is the best available proxy for
		// capability within a task class.
		value *= 0.7 + 0.3*min1(seed.ParamsB/14)
	}

	return int(value), why
}

func min1(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < 0 {
		return 0
	}
	return v
}

// commands produces copy-and-run invocations for both serving stacks.
func commands(seed SeedModel, quant string, ctx int) (run, pull string) {
	if seed.OllamaTag != "" {
		pull = "ollama pull " + seed.OllamaTag
	}
	if seed.GGUFRepo == "" {
		if seed.OllamaTag != "" {
			run = "ollama run " + seed.OllamaTag
		}
		return run, pull
	}

	file := fmt.Sprintf("%s-%s.gguf", strings.ReplaceAll(seed.Display, " ", "-"), quant)
	run = fmt.Sprintf(`llama-server \
  --model ~/models/%s \
  --alias %s \
  --n-gpu-layers 99 \
  --ctx-size %d \
  --flash-attn on \
  --cache-type-k q8_0 --cache-type-v q8_0 \
  --jinja \
  --host 0.0.0.0 --port 8080`, file, seed.ID, ctx)
	return run, pull
}

// summarize writes the paragraph shown above the table.
func summarize(plan *Plan, req PlanRequest, hw *Hardware) string {
	var b strings.Builder

	gpu := (*GPU)(nil)
	if hw != nil {
		gpu = hw.PrimaryGPU()
	}
	if gpu == nil {
		b.WriteString("No GPU was detected, so every recommendation below assumes CPU inference — expect single-digit tokens per second. ")
	} else {
		fmt.Fprintf(&b, "On the %s with %.1fGB free of %.1fGB VRAM, at %d tokens of context: ",
			gpu.Name, float64(gpu.FreeMB)/1024, float64(gpu.TotalMB)/1024, req.ContextTokens)
	}

	if len(plan.Recommendations) == 0 {
		b.WriteString("no curated model matches those constraints. Try the Explore page to search the Hugging Face Hub directly.")
		return b.String()
	}

	top := plan.Recommendations[0]
	fmt.Fprintf(&b, "%s at %s is the best match. %s", top.Display, top.Quant, top.Note)

	// Name the best fully-resident option too, when the winner is not one.
	if top.Fit != nil && (top.Fit.Verdict == VerdictPartial || top.Fit.Verdict == VerdictNo) {
		for _, r := range plan.Recommendations[1:] {
			if r.Fit != nil && r.Fit.Verdict == VerdictFits {
				fmt.Fprintf(&b, " If you would rather not pay the CPU-offload penalty, %s at %s runs entirely on the GPU at roughly %.0f tokens/sec.",
					r.Display, r.Quant, r.Fit.TokensPerSec)
				break
			}
		}
	}
	return b.String()
}
