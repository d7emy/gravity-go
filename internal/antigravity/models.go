package antigravity

// ModelInfo is one entry in the /v1/models listing.
type ModelInfo struct {
	ID   string
	Name string
}

// AvailableModels is the catalogue advertised to clients.
var AvailableModels = []ModelInfo{
	{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5"},
	{ID: "claude-sonnet-4-5-thinking", Name: "Claude Sonnet 4.5 (Thinking)"},
	{ID: "claude-opus-4-5-thinking", Name: "Claude Opus 4.5 (Thinking)"},
	{ID: "claude-opus-4-6", Name: "Claude Opus 4.6"},
	{ID: "claude-opus-4-6-thinking", Name: "Claude Opus 4.6 (Thinking)"},

	{ID: "gemini-3-pro-high", Name: "Gemini 3 Pro (High)"},
	{ID: "gemini-3-pro-low", Name: "Gemini 3 Pro (Low)"},
	{ID: "gemini-3.1-pro-high", Name: "Gemini 3.1 Pro (High)"},
	{ID: "gemini-3-flash", Name: "Gemini 3 Flash"},
	{ID: "gemini-3.7-flash", Name: "Gemini 3.7 Flash"},
	{ID: "gemini-3.7-flash-low", Name: "Gemini 3.7 Flash (Low)"},
	{ID: "gemini-3.7-flash-medium", Name: "Gemini 3.7 Flash (Medium)"},
	{ID: "gemini-3.7-flash-high", Name: "Gemini 3.7 Flash (High)"},
	{ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash"},
	{ID: "gemini-3.8-flash-low", Name: "Gemini 3.8 Flash (Low)"},
	{ID: "gemini-3.8-flash-medium", Name: "Gemini 3.8 Flash (Medium)"},
	{ID: "gemini-3.8-flash-high", Name: "Gemini 3.8 Flash (High)"},

	// Cyber is Fairwind-gated server-side: without an entitled project every
	// one of these 404s (verified 2026-09-05 on three accounts). They stay out
	// of visibleModels so listings never advertise them; they are listed here
	// so an entitled token can call them by id.
	{ID: "gemini-3.8-flash-cyber", Name: "Gemini 3.8 Flash Cyber"},
	{ID: "gemini-3.8-flash-cyber-low", Name: "Gemini 3.8 Flash Cyber (Low)"},
	{ID: "gemini-3.8-flash-cyber-medium", Name: "Gemini 3.8 Flash Cyber (Medium)"},
	{ID: "gemini-3.8-flash-cyber-high", Name: "Gemini 3.8 Flash Cyber (High)"},

	{ID: "gpt-oss-120b", Name: "GPT-OSS 120B (Medium)"},
}

// visibleModels is the whitelist actually exposed on the model endpoints.
// Everything else stays callable by id but is hidden from listings.
var visibleModels = map[string]bool{
	"gemini-3.8-flash-high":    true,
	"gemini-3.7-flash-high":    true,
	"gemini-3.1-pro-high":      true,
	"claude-opus-4-6-thinking": true,
}

// IsVisibleModel reports whether a model id appears in listings.
func IsVisibleModel(id string) bool { return visibleModels[id] }

// FilterVisibleModels narrows a catalogue to the visible whitelist.
func FilterVisibleModels(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		if visibleModels[model.ID] {
			out = append(out, model)
		}
	}
	return out
}

// modelMapping translates a client-facing model id into the upstream id.
// Several client ids deliberately collapse onto one upstream model: the upstream
// name is rollout-dependent (3.7 Flash only exposes "-tiered", for instance) and
// the thinking level travels separately in thinkingConfig.
var modelMapping = map[string]string{
	"claude-sonnet-4-5":          "claude-sonnet-4-5",
	"claude-sonnet-4-5-thinking": "claude-sonnet-4-5-thinking",
	"claude-opus-4-5-thinking":   "claude-opus-4-5-thinking",
	"claude-opus-4-6":            "claude-opus-4-6-thinking",
	"claude-opus-4-6-thinking":   "claude-opus-4-6-thinking",
	"claude-sonnet-4.5":          "claude-sonnet-4-5",
	"claude-sonnet-4.5-thinking": "claude-sonnet-4-5-thinking",
	"claude-opus-4.5-thinking":   "claude-opus-4-5-thinking",
	"claude-opus-4.6":            "claude-opus-4-6-thinking",
	"claude-opus-4.6-thinking":   "claude-opus-4-6-thinking",
	"claude-sonnet-4-5-20251001": "claude-sonnet-4-5",
	"claude-sonnet-4-6":          "claude-sonnet-4-6",

	"gemini-3-pro-high":       "gemini-3.1-pro-low",
	"gemini-3-pro-low":        "gemini-3.1-pro-low",
	"gemini-3.1-pro-high":     "gemini-pro-agent",
	"gemini-3.1-pro-low":      "gemini-3.1-pro-low",
	"gemini-2.5-pro":          "gemini-2.5-pro",
	"gemini-3-flash":          "gemini-3-flash",
	"gemini-3.7-flash":        "gemini-3.7-flash-tiered",
	"gemini-3.7-flash-low":    "gemini-3.7-flash-tiered",
	"gemini-3.7-flash-medium": "gemini-3.7-flash-tiered",
	"gemini-3.7-flash-high":   "gemini-3.7-flash-tiered",
	"gemini-3.7-flash-tiered": "gemini-3.7-flash-tiered",

	// 3.8 mirrors 3.7: upstream publishes only the "-tiered" variant, so every
	// thinking level collapses onto it and the -low/-medium/-high suffix is what
	// selects the thinking level in buildUpstreamRequest. Note 3.8 is absent from
	// fetchAvailableModels but serves normally -- the catalogue lags the rollout,
	// so a model missing from it is not proof it is unavailable.
	"gemini-3.8-flash":        "gemini-3.8-flash-tiered",
	"gemini-3.8-flash-low":    "gemini-3.8-flash-tiered",
	"gemini-3.8-flash-medium": "gemini-3.8-flash-tiered",
	"gemini-3.8-flash-high":   "gemini-3.8-flash-tiered",
	"gemini-3.8-flash-tiered": "gemini-3.8-flash-tiered",

	// Cyber passthrough: no public wire id is documented and no
	// fetchAvailableModels dump has shown one, so every variant maps onto
	// itself and the caller's id reaches upstream verbatim. An entitled
	// (Fairwind) project can therefore use whatever wire id Google issued it;
	// everyone else gets upstream's 404. Revisit once a dump confirms a real
	// wire id (e.g. a "-tiered" collapse like 3.7/3.8 flash).
	"gemini-3.8-flash-cyber":        "gemini-3.8-flash-cyber",
	"gemini-3.8-flash-cyber-low":    "gemini-3.8-flash-cyber-low",
	"gemini-3.8-flash-cyber-medium": "gemini-3.8-flash-cyber-medium",
	"gemini-3.8-flash-cyber-high":   "gemini-3.8-flash-cyber-high",
	"gemini-3.8-flash-cyber-tiered": "gemini-3.8-flash-cyber-tiered",
	"gemini-pro-agent":              "gemini-pro-agent",
	"gemini-pro-agent-high":         "gemini-pro-agent",
	"gemini-3.1-flash-image":        "gemini-3.1-flash-image",

	"gpt-oss-120b":        "gpt-oss-120b-medium",
	"gpt-oss-120b-medium": "gpt-oss-120b-medium",
}

// UpstreamModelName maps a client model id to the upstream name, passing through
// anything unrecognised.
func UpstreamModelName(userModel string) string {
	if mapped, ok := modelMapping[userModel]; ok {
		return mapped
	}
	return userModel
}

// openAIModelMapping lets OpenAI-shaped clients address the proxy with familiar
// model names.
var openAIModelMapping = map[string]string{
	"gpt-4":         "claude-sonnet-4-5",
	"gpt-4o":        "claude-sonnet-4-5",
	"gpt-4-turbo":   "claude-sonnet-4-5",
	"gpt-3.5-turbo": "gemini-2.0-flash-exp",
	"o1":            "claude-sonnet-4-5-thinking",
	"o1-mini":       "gemini-2.0-flash-exp",
}

// MapOpenAIModel translates an OpenAI model id to an internal one.
func MapOpenAIModel(model string) string {
	if mapped, ok := openAIModelMapping[model]; ok {
		return mapped
	}
	return model
}
