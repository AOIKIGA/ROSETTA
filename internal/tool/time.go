package tool

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"
	_ "time/tzdata" // Keep IANA zones available in a standalone binary.

	"rosetta/internal/llm"
)

// Time's optional clock is injected by tests; leave it nil to use time.Now.
type Time struct{ Now func() time.Time }

func (Time) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        "get_time",
		Description: "Query the actual current date and time in an IANA timezone. Default: Asia/Shanghai.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"timezone":{"type":"string","description":"IANA timezone, for example Asia/Shanghai or UTC"}},"additionalProperties":false}`),
	}
}

func (t Time) Execute(ctx context.Context, arguments string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	invalid := Result{IsError: true, Code: "invalid_arguments", Content: "Expected one JSON object with an optional string timezone (IANA name)."}
	if !strings.HasPrefix(strings.TrimSpace(arguments), "{") {
		return invalid, nil
	}
	var args struct {
		Timezone *string `json:"timezone"`
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return invalid, nil
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalid, nil
	}
	// A supplied null is not a valid timezone string.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal([]byte(arguments), &fields)
	if raw, supplied := fields["timezone"]; supplied && string(raw) == "null" {
		return invalid, nil
	}
	zone := "Asia/Shanghai"
	if args.Timezone != nil {
		zone = *args.Timezone
	}
	// time.LoadLocation also accepts absolute paths. Accept only timezone names,
	// never paths, Local (machine dependent), or names with empty/dot components.
	if zone == "" || zone == "Local" || len(zone) > 128 {
		return invalid, nil
	}
	for _, component := range strings.Split(zone, "/") {
		if component == "" || component == "." || component == ".." {
			return invalid, nil
		}
		for _, c := range component {
			if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '+') {
				return invalid, nil
			}
		}
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return Result{IsError: true, Code: "invalid_timezone", Content: "Unknown IANA timezone. Try Asia/Shanghai or UTC."}, nil
	}
	now := t.Now
	if now == nil {
		now = time.Now
	}
	data, _ := json.Marshal(struct {
		Timezone string `json:"timezone"`
		Time     string `json:"time"`
	}{zone, now().In(location).Format(time.RFC3339)})
	return Result{Content: string(data)}, nil
}
