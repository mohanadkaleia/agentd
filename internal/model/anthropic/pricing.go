package anthropic

type price struct {
	inputCentsPerMTok  int64
	outputCentsPerMTok int64
}

var prices = map[string]price{
	"claude-fable-5-1":  {1000, 5000},
	"claude-fable-5":    {1000, 5000},
	"claude-opus-5-5":   {400, 2000},
	"claude-opus-5":     {500, 2500},
	"claude-opus-4-8":   {500, 2500},
	"claude-opus-4-7":   {500, 2500},
	"claude-sonnet-5-5": {200, 1000},
	"claude-sonnet-5":   {200, 1000},
	"claude-sonnet-4-6": {300, 1500},
	"claude-haiku-5-5":  {10, 50},
}

var mostExpensive = price{1000, 5000}

const haikuLongPromptTokens = 100_000

func CostMicros(modelID string, inputTokens, outputTokens int64) int64 {
	p, ok := prices[modelID]
	if !ok {
		p = mostExpensive
	}
	if modelID == "claude-haiku-5-5" && inputTokens > haikuLongPromptTokens {
		p = price{50, 250}
	}
	return ceilDiv(inputTokens*p.inputCentsPerMTok+outputTokens*p.outputCentsPerMTok, 100)
}

func ceilDiv(a, b int64) int64 {
	return (a + b - 1) / b
}
