package agent

import (
	"encoding/json"

	"rosetta/internal/llm"
)

// prepareInput removes only whole old turns, oldest first, on every model round.
// History stays in SQLite even when it no longer fits this request.
func (r *Runtime) prepareInput(history []llm.HistoryTurn, current []llm.Message, specs []llm.ToolSpec) (llm.Request, []llm.HistoryTurn, error) {
	for {
		messages := []llm.Message{{Role: "system", Content: systemPrompt}}
		for _, turn := range history {
			messages = append(messages, turn.Messages...)
		}
		messages = append(messages, current...)
		input := llm.Request{Messages: messages, Tools: specs}
		encoded, err := json.Marshal(input)
		size := len(encoded)
		if err == nil {
			// Some adapters add wire fields. Measure them too rather than letting
			// historical tool wrappers push an otherwise valid input over budget.
			if sizer, ok := r.provider.(llm.InputSizer); ok {
				wireSize, wireErr := sizer.InputSize(input)
				size, err = max(size, wireSize), wireErr
			}
		}
		if err != nil {
			return llm.Request{}, nil, &llm.Error{Code: "input_limit"}
		}
		if size <= MaxInputBytes {
			return input, history, nil
		}
		if len(history) == 0 {
			return llm.Request{}, nil, &llm.Error{Code: "input_limit"}
		}
		history = history[1:]
	}
}
