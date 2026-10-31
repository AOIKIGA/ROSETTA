package tool

import (
	"encoding/json"
	"io"
	"strings"
)

func decodeObject(arguments string, target any) bool {
	if len(arguments) > 16<<10 || !strings.HasPrefix(strings.TrimSpace(arguments), "{") {
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF
}

func failure(code, message string) Result { return Result{Content: message, Code: code, IsError: true} }

func fitsWire(result Result) bool {
	data, err := json.Marshal(result)
	return err == nil && len(data) <= MaxOutputBytes
}
