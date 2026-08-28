package nvidia

import (
	"encoding/json"
)

// NormalizeRequestBody folds common client aliases into chat_template_kwargs
// for known reasoning models. Stream-options handling lives in forceStreamFlag
// (called by preparePayload right after this) — keeping it there means this
// function has a single responsibility and there's one canonical place that
// owns stream_options.
func NormalizeRequestBody(body []byte) ([]byte, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	normalizeThinking(raw)
	return json.Marshal(raw)
}
