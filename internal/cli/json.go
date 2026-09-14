package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Keep API amounts and other integers exact when decoding dynamic JSON values.
// Decoding into float64 silently rounds integers above 2^53.
func decodeJSONNumbers(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values")
	}
	return nil
}
