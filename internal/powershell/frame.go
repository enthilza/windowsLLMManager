package powershell

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

func readFrame(r io.Reader, id string, maxBytes int) (framedResult, error) {
	begin := "___WLM_BEGIN_" + id + "___"
	end := "___WLM_END_" + id + "___"
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 64*1024)
	var result framedResult
	var output64, stderr64 strings.Builder
	inFrame := false
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if !inFrame {
			if line == begin {
				inFrame = true
				continue
			}
			if result.RawPrelude == "" {
				result.RawPrelude = line
			} else if len(result.RawPrelude) < maxBytes {
				result.RawPrelude += "\n" + line
			}
			continue
		}
		if line == end {
			if result.Meta.Format == "" {
				return framedResult{}, errors.New("frame did not contain metadata")
			}
			var err error
			result.Output, err = base64.StdEncoding.DecodeString(output64.String())
			if err != nil {
				return framedResult{}, fmt.Errorf("decode output frame: %w", err)
			}
			result.Stderr, err = base64.StdEncoding.DecodeString(stderr64.String())
			if err != nil {
				return framedResult{}, fmt.Errorf("decode stderr frame: %w", err)
			}
			return result, nil
		}
		switch {
		case strings.HasPrefix(line, "M:"):
			meta, err := decodeMeta(strings.TrimPrefix(line, "M:"))
			if err != nil {
				return framedResult{}, fmt.Errorf("decode frame metadata: %w", err)
			}
			result.Meta = meta
		case strings.HasPrefix(line, "O:"):
			if output64.Len() < base64.StdEncoding.EncodedLen(maxBytes)+frameChunkChars {
				output64.WriteString(strings.TrimPrefix(line, "O:"))
			}
		case strings.HasPrefix(line, "E:"):
			if stderr64.Len() < base64.StdEncoding.EncodedLen(maxBytes)+frameChunkChars {
				stderr64.WriteString(strings.TrimPrefix(line, "E:"))
			}
		default:
			return framedResult{}, fmt.Errorf("invalid frame line")
		}
	}
	if err := scanner.Err(); err != nil {
		return framedResult{}, err
	}
	return framedResult{}, io.ErrUnexpectedEOF
}
