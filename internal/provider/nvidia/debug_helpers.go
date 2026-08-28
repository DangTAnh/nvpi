package nvidia

import (
	"encoding/json"

	"github.com/tidwall/gjson"
)

// extractRequestID pulls the Anthropic `messages` request id out of the
// original request body so the SSE debug logs can correlate a turn with
// the Claude Code session that produced it. Empty when the id is absent
// or the body is not valid JSON.
func extractRequestID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if !json.Valid(body) {
		// Fall back to gjson which is more forgiving on partial JSON.
		return gjson.GetBytes(body, "metadata.user_id").String()
	}
	return gjson.GetBytes(body, "metadata.user_id").String()
}
