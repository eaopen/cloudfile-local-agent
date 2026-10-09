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
	Type        string `json:"type"`
	Path        string `json:"path,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	Server      string `json:"server,omitempty"`
	Ticket      string `json:"ticket,omitempty"`
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	RepoID      string `json:"repo_id,omitempty"`
	Mode        string `json:"mode,omitempty"`
	LocalAction string `json:"local_action,omitempty"`
}

type Response struct {
	OK               bool     `json:"ok"`
	Error            string   `json:"error,omitempty"`
	Version          string   `json:"version,omitempty"`
	ExecutablePath   string   `json:"executable_path,omitempty"`
	Applications     []string `json:"applications,omitempty"`
	WorkspaceRoot    string   `json:"workspace_root,omitempty"`
	CanOpenWorkspace bool     `json:"can_open_workspace,omitempty"`
	UpdateAvailable  bool     `json:"update_available,omitempty"`
	LatestVersion    string   `json:"latest_version,omitempty"`
	LatestNotes      string   `json:"latest_notes,omitempty"`
	UpdateSourceSet  bool     `json:"update_source_set,omitempty"`
	// Extension* describe the browser extension, so the extension can drive its
	// own update prompt over Native Messaging instead of a cross-origin fetch
	// (which needs CORS and silently fails without it). An available version is
	// only ever a prompt: nothing is refused because of a version comparison.
	ExtensionLatestVersion    string `json:"extension_latest_version,omitempty"`
	ExtensionInstalledVersion string `json:"extension_installed_version,omitempty"`
	ExtensionNotes            string `json:"extension_notes,omitempty"`
	Mode                      string `json:"mode,omitempty"`
	LocalExists               bool   `json:"local_exists,omitempty"`
	LocalPath                 string `json:"local_path,omitempty"`
	LocalHash                 string `json:"local_hash,omitempty"`
	LocalDigest               string `json:"local_digest,omitempty"`
	LocalSize                 int64  `json:"local_size,omitempty"`
	LocalMTime                int64  `json:"local_mtime,omitempty"`
	// Baseline* describe the fingerprint written when the local copy was
	// downloaded. They let the page tell "the server moved on" from "the user
	// changed the local copy" without keeping any state in browser storage.
	BaselineExists bool   `json:"baseline_exists,omitempty"`
	BaselineFileID string `json:"baseline_file_id,omitempty"`
	BaselineDigest string `json:"baseline_digest,omitempty"`
	BaselineAt     int64  `json:"baseline_at,omitempty"`
	// LocalChanged is only meaningful when BaselineExists is true: a false
	// baseline means the local copy cannot be compared, not that it matches.
	LocalChanged bool `json:"local_changed,omitempty"`
}

// mode returns the mirror subtree a request addresses. Anything that is not an
// explicit view request is treated as editable, which is the conservative
// default: the edit subtree is the only one that never gets overwritten by a
// view open.
func (r Request) MirrorMode() string {
	if r.Mode == "local-view" {
		return "local-view"
	}
	return "local-edit"
}

func (r Request) Valid() bool {
	if r.Mode != "" && r.Mode != "local-view" && r.Mode != "local-edit" {
		return false
	}
	if r.Type == "status" {
		return r.Path == ""
	}
	if r.Type == "update" {
		return r.Path == ""
	}
	if r.Type == "check_update" {
		return r.Path == ""
	}
	if r.Type == "update_extension" {
		return r.Path == ""
	}
	if r.Type == "open_workspace" {
		return r.Protocol == "" && r.Server == "" &&
			r.Ticket == "" && r.ExpiresAt == 0
	}
	if r.Type == "query_local_file" {
		return r.RepoID != "" && r.Path != ""
	}
	// query_local_folder asks whether a library already has a local folder for a
	// mode; it addresses the folder, not one file, so it carries no path.
	if r.Type == "query_local_folder" {
		return r.RepoID != "" && r.Protocol == "" && r.Server == "" &&
			r.Ticket == "" && r.ExpiresAt == 0
	}
	if r.Type == "open_session" {
		return r.Path == "" && r.Protocol != "" && r.Server != "" &&
			r.Ticket != "" && r.ExpiresAt > 0
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
