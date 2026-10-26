package protocol

import (
	"encoding/json"
	"errors"
	"io"
)

// MaxLine bounds one JSON line.
const MaxLine = 1 << 20

// ErrLineTooLong is returned for a JSON line longer than MaxLine.
var ErrLineTooLong = errors.New("line too long")

// ReadLine reads one '\n'-terminated line a byte at a time, so nothing past
// it is consumed: after a Hello the same reader carries frames or raw bytes.
// The newline is not returned.
func ReadLine(r io.Reader) ([]byte, error) {
	var line []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				return line, nil
			}
			if len(line) >= MaxLine {
				return nil, ErrLineTooLong
			}
			line = append(line, b[0])
			continue
		}
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
}

// WriteJSON writes v as one line in a single Write call.
func WriteJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadHello reads the Hello (or Error) that opens a connection.
func ReadHello(r io.Reader) (Hello, error) {
	line, err := ReadLine(r)
	if err != nil {
		return Hello{}, err
	}
	var env struct {
		Hello
		Message string `json:"message"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return Hello{}, err
	}
	switch env.Type {
	case TypeHello:
		return env.Hello, nil
	case TypeError:
		return Hello{}, &RemoteError{Message: env.Message}
	default:
		return Hello{}, errors.New("expected hello, got " + env.Type)
	}
}

// RemoteError is an Error message the peer sent.
type RemoteError struct{ Message string }

func (e *RemoteError) Error() string { return "agent: " + e.Message }
