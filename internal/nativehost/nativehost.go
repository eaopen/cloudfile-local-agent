package nativehost

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

const maxMessageBytes = 1024 * 1024

type Request struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
}

type Response struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
}

func (r Request) Valid() bool {
	if r.Type == "status" {
		return r.Path == ""
	}
	return r.Type == "open_session_file" && r.Path != "" &&
		strings.EqualFold(filepath.Ext(r.Path), ".cloudfile")
}

func Serve(input io.Reader, output io.Writer, handle func(Request) Response) error {
	reader := bufio.NewReader(input)
	for {
		var size uint32
		if err := binary.Read(reader, binary.LittleEndian, &size); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if size == 0 || size > maxMessageBytes {
			return fmt.Errorf("native message length is invalid")
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return err
		}
		request := Request{}
		response := Response{}
		if err := json.Unmarshal(payload, &request); err != nil {
			response = Response{Error: "invalid native message"}
		} else if !request.Valid() {
			response = Response{Error: "invalid native message"}
		} else {
			response = handle(request)
		}
		if err := write(output, response); err != nil {
			return err
		}
	}
}

func write(output io.Writer, response Response) error {
	payload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if len(payload) > maxMessageBytes {
		return fmt.Errorf("native response is too large")
	}
	if err := binary.Write(output, binary.LittleEndian, uint32(len(payload))); err != nil {
		return err
	}
	_, err = output.Write(payload)
	return err
}
