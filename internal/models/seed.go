package models

// Task is a job a user wants a model for. The planner ranks candidates per
// task, because "best model" is meaningless without one — the model that
// writes the best report is not the model that emits the most reliable tool
// calls, and on a memory-constrained card you cannot have both loaded.
type Task string

const (
	// TaskGeneral is everyday assistant work.
	TaskGeneral Task = "general"
	// TaskAgent is tool calling: the wintermute rename flow, anything that
	// must emit structured calls reliably.
	TaskAgent Task = "agent"
	// TaskDocuments is long-form prose: reports, summaries, documentation.
	TaskDocuments Task = "documents"
	// TaskCoding is writing and editing code.
	TaskCoding Task = "coding"
	// TaskLongContext is work dominated by how much source material fits.
	TaskLongContext Task = "long_context"
	// TaskVision is image understanding, including camera frame analysis.
	TaskVision Task = "vision"
	// TaskEmbedding is retrieval and semantic search.
	TaskEmbedding Task = "embedding"
	// TaskReasoning is multi-step problem solving.
	TaskReasoning Task = "reasoning"
)

// AllTasks lists the task classes in the order the UI presents them.
var AllTasks = []Task{
	TaskGeneral, TaskAgent, TaskDocuments, TaskCoding,
	TaskLongContext, TaskVision, TaskReasoning, TaskEmbedding,
}

// TaskInfo describes a task for the UI.
type TaskInfo struct {
	Task  Task   `json:"task"`
	Label string `json:"label"`
	Blurb string `json:"blurb"`
}

// TaskCatalog is the task list with human-facing copy.
var TaskCatalog = []TaskInfo{
	{TaskGeneral, "General assistant", "Everyday questions, drafting, explanation."},
	{TaskAgent, "Tool calling / agent", "Reliably emitting structured tool calls — what wintermute's own flows need."},
	{TaskDocuments, "Document generation", "Long-form prose: reports, summaries, documentation."},
	{TaskCoding, "Code", "Writing, editing and reviewing source."},
	{TaskLongContext, "Long context", "Work limited by how much source material fits in the window."},
	{TaskVision, "Vision", "Understanding images and camera frames."},
	{TaskReasoning, "Reasoning", "Multi-step problems that reward explicit thinking."},
	{TaskEmbedding, "Embeddings", "Retrieval and semantic search."},
}

// SeedModel is curated knowledge about a model family.
//
// This exists so recommendations work with no network access and without
// trusting a download counter as a proxy for quality. It is a floor, not a
// ceiling: the Explore page queries the Hugging Face Hub live, so models newer
// than this list are still discoverable. Scores are 0-100 and are relative
// judgements within the set, not benchmark numbers.
type SeedModel struct {
	ID           string       `json:"id"`
	Display      string       `json:"display"`
	Family       string       `json:"family"`
	ParamsB      float64      `json:"params_b"`
	ActiveB      float64      `json:"active_params_b,omitempty"`
	License      string       `json:"license"`
	MaxCtx       int          `json:"max_ctx"`
	Capabilities []Capability `json:"capabilities,omitempty"`
	// GGUFRepo is where to get quantized weights, and OllamaTag is the pull
	// name. Either may be empty.
	GGUFRepo  string `json:"gguf_repo,omitempty"`
	OllamaTag string `json:"ollama_tag,omitempty"`
	// Scores rates the model per task.
	Scores map[Task]int `json:"scores"`
	// Note is the one-line reason to pick this model.
	Note string `json:"note"`
}

// Has reports whether the seed model declares a capability.
func (s SeedModel) Has(c Capability) bool {
	for _, got := range s.Capabilities {
		if got == c {
			return true
		}
	}
	return false
}

// Seed is the curated set, weighted toward what is realistic on a single
// consumer GPU. Larger models are included so the planner can explain why they
// do not fit rather than silently omitting them.
var Seed = []SeedModel{
	{
		ID: "qwen3-8b", Display: "Qwen3 8B", Family: "Qwen3", ParamsB: 8.2,
		License: "Apache-2.0", MaxCtx: 32768,
		Capabilities: []Capability{CapTools, CapReasoning},
		GGUFRepo:     "bartowski/Qwen_Qwen3-8B-GGUF", OllamaTag: "qwen3:8b",
		Scores: map[Task]int{
			TaskGeneral: 85, TaskAgent: 88, TaskDocuments: 78, TaskCoding: 80,
			TaskLongContext: 72, TaskReasoning: 82,
		},
		Note: "The best all-round fit for an 8GB card: full GPU offload at Q4_K_M with room for 16K context, and dependable tool calling.",
	},
	{
		ID: "qwen3-4b", Display: "Qwen3 4B", Family: "Qwen3", ParamsB: 4.0,
		License: "Apache-2.0", MaxCtx: 32768,
		Capabilities: []Capability{CapTools, CapReasoning},
		GGUFRepo:     "bartowski/Qwen_Qwen3-4B-GGUF", OllamaTag: "qwen3:4b",
		Scores: map[Task]int{
			TaskGeneral: 74, TaskAgent: 78, TaskDocuments: 66, TaskCoding: 70,
			TaskLongContext: 82, TaskReasoning: 72,
		},
		Note: "Half the memory of the 8B, so it leaves room for a long context window. The pick when source material matters more than polish.",
	},
	{
		ID: "qwen3-14b", Display: "Qwen3 14B", Family: "Qwen3", ParamsB: 14.8,
		License: "Apache-2.0", MaxCtx: 32768,
		Capabilities: []Capability{CapTools, CapReasoning},
		GGUFRepo:     "bartowski/Qwen_Qwen3-14B-GGUF", OllamaTag: "qwen3:14b",
		Scores: map[Task]int{
			TaskGeneral: 90, TaskAgent: 90, TaskDocuments: 86, TaskCoding: 85,
			TaskLongContext: 74, TaskReasoning: 88,
		},
		Note: "Noticeably stronger than the 8B, but at Q4_K_M it needs roughly 9GB — over the line on an 8GB card without heavy context cuts.",
	},
	{
		ID: "gemma-3-4b", Display: "Gemma 3 4B", Family: "Gemma 3", ParamsB: 4.3,
		License: "Gemma", MaxCtx: 131072,
		Capabilities: []Capability{CapTools, CapVision},
		GGUFRepo:     "bartowski/google_gemma-3-4b-it-GGUF", OllamaTag: "gemma3:4b",
		Scores: map[Task]int{
			TaskGeneral: 72, TaskAgent: 68, TaskDocuments: 70, TaskCoding: 60,
			TaskLongContext: 84, TaskVision: 76,
		},
		Note: "Small, fast and multimodal. The default choice for camera-frame analysis, where you want an image described in a second or two.",
	},
	{
		ID: "gemma-3-12b-qat", Display: "Gemma 3 12B (QAT)", Family: "Gemma 3", ParamsB: 12.2,
		License: "Gemma", MaxCtx: 131072,
		Capabilities: []Capability{CapTools, CapVision},
		GGUFRepo:     "google/gemma-3-12b-it-qat-q4_0-gguf", OllamaTag: "gemma3:12b",
		Scores: map[Task]int{
			TaskGeneral: 86, TaskAgent: 74, TaskDocuments: 90, TaskCoding: 72,
			TaskLongContext: 80, TaskVision: 84,
		},
		Note: "The best prose quality that still fits 8GB, and the quantization-aware release holds up at 4-bit far better than a naive post-training quant. Needs a reduced context to fit.",
	},
	{
		ID: "llama-3.1-8b", Display: "Llama 3.1 8B", Family: "Llama 3", ParamsB: 8.0,
		License: "Llama 3.1 Community", MaxCtx: 131072,
		Capabilities: []Capability{CapTools},
		GGUFRepo:     "bartowski/Meta-Llama-3.1-8B-Instruct-GGUF", OllamaTag: "llama3.1:8b",
		Scores: map[Task]int{
			TaskGeneral: 78, TaskAgent: 76, TaskDocuments: 76, TaskCoding: 70,
			TaskLongContext: 80, TaskReasoning: 70,
		},
		Note: "The well-understood baseline. Not the strongest at any one thing any more, but nothing in the ecosystem is untested against it.",
	},
	{
		ID: "mistral-nemo-12b", Display: "Mistral Nemo 12B", Family: "Mistral", ParamsB: 12.2,
		License: "Apache-2.0", MaxCtx: 131072,
		Capabilities: []Capability{CapTools},
		GGUFRepo:     "bartowski/Mistral-Nemo-Instruct-2407-GGUF", OllamaTag: "mistral-nemo:12b",
		Scores: map[Task]int{
			TaskGeneral: 80, TaskAgent: 76, TaskDocuments: 84, TaskCoding: 70,
			TaskLongContext: 82,
		},
		Note: "A strong writer with a permissive licence and a long native context. Tight on 8GB at Q4_K_M.",
	},
	{
		ID: "phi-4-mini", Display: "Phi-4 mini", Family: "Phi", ParamsB: 3.8,
		License: "MIT", MaxCtx: 131072,
		Capabilities: []Capability{CapTools, CapReasoning},
		OllamaTag:    "phi4-mini",
		Scores: map[Task]int{
			TaskGeneral: 70, TaskAgent: 64, TaskDocuments: 62, TaskCoding: 68,
			TaskLongContext: 76, TaskReasoning: 78,
		},
		Note: "Punches above its size on reasoning for the memory it uses, and MIT-licensed.",
	},
	{
		ID: "granite-4-8b", Display: "Granite 4 8B", Family: "Granite", ParamsB: 8.0,
		License: "Apache-2.0", MaxCtx: 131072,
		Capabilities: []Capability{CapTools},
		OllamaTag:    "granite4",
		Scores: map[Task]int{
			TaskGeneral: 74, TaskAgent: 86, TaskDocuments: 72, TaskCoding: 72,
			TaskLongContext: 78,
		},
		Note: "Explicitly trained for function calling, which shows in agent work. Worth trying if another model's tool calls are unreliable.",
	},
	{
		ID: "qwen3-coder-30b-a3b", Display: "Qwen3-Coder 30B A3B", Family: "Qwen3", ParamsB: 30.5, ActiveB: 3.3,
		License: "Apache-2.0", MaxCtx: 262144,
		Capabilities: []Capability{CapTools},
		GGUFRepo:     "unsloth/Qwen3-Coder-30B-A3B-Instruct-GGUF", OllamaTag: "qwen3-coder:30b",
		Scores: map[Task]int{
			TaskGeneral: 82, TaskAgent: 88, TaskDocuments: 70, TaskCoding: 94,
			TaskLongContext: 86,
		},
		Note: "Far too large for 8GB of VRAM, but only ~3B parameters are active per token, so CPU offload stays usable in a way a dense 30B never would.",
	},
	{
		ID: "qwen2.5-vl-7b", Display: "Qwen2.5-VL 7B", Family: "Qwen-VL", ParamsB: 8.3,
		License: "Apache-2.0", MaxCtx: 32768,
		Capabilities: []Capability{CapVision, CapTools},
		OllamaTag:    "qwen2.5vl:7b",
		Scores: map[Task]int{
			TaskGeneral: 72, TaskAgent: 68, TaskDocuments: 64, TaskVision: 90,
			TaskLongContext: 70,
		},
		Note: "The strongest open vision model that fits an 8GB card. Good at localising events in a frame, which is what a camera pipeline needs.",
	},
	{
		ID: "qwen2.5-vl-3b", Display: "Qwen2.5-VL 3B", Family: "Qwen-VL", ParamsB: 3.8,
		License: "Apache-2.0", MaxCtx: 32768,
		Capabilities: []Capability{CapVision},
		OllamaTag:    "qwen2.5vl:3b",
		Scores: map[Task]int{
			TaskGeneral: 62, TaskVision: 82, TaskLongContext: 68,
		},
		Note: "Nearly as capable as the 7B on scene description at half the memory — the right size when the GPU is also serving a text model.",
	},
	{
		ID: "nomic-embed-text", Display: "Nomic Embed Text", Family: "Nomic", ParamsB: 0.14,
		License: "Apache-2.0", MaxCtx: 8192,
		Capabilities: []Capability{CapEmbedding},
		OllamaTag:    "nomic-embed-text",
		Scores:       map[Task]int{TaskEmbedding: 86},
		Note:         "Small, fast retrieval embeddings. Costs so little VRAM it can stay resident alongside a chat model.",
	},
	{
		ID: "bge-m3", Display: "BGE-M3", Family: "BAAI", ParamsB: 0.57,
		License: "MIT", MaxCtx: 8192,
		Capabilities: []Capability{CapEmbedding},
		OllamaTag:    "bge-m3",
		Scores:       map[Task]int{TaskEmbedding: 90},
		Note:         "Stronger multilingual retrieval than nomic-embed at a still-negligible memory cost.",
	},
}

// SeedByID looks up one curated model.
func SeedByID(id string) (SeedModel, bool) {
	for _, m := range Seed {
		if m.ID == id {
			return m, true
		}
	}
	return SeedModel{}, false
}
