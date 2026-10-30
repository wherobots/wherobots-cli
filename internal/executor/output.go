package executor

import (
	"bytes"
	"fmt"
	"io"

	"github.com/tidwall/gjson"
)

func WriteSuccessResponse(out io.Writer, body []byte) error {
	// Only 2xx responses reach here, and a 204 or an empty 202 is still a success.
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if !gjson.ValidBytes(body) {
		return fmt.Errorf("response body is not valid JSON")
	}
	_, err := out.Write(body)
	return err
}
