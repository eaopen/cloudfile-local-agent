package editing

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// FileJournal requires an existing private session directory. Temp + rename
// prevents a process crash from replacing the recovery state with partial JSON.
type FileJournal struct{ Path string }

func (j FileJournal) Save(state State) error {
	dir := filepath.Dir(j.Path)
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private session directory required")
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".edit-intent-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return renameJournal(name, j.Path)
}
func (j FileJournal) Load() (State, error) {
	var state State
	info, err := os.Lstat(j.Path)
	if err != nil {
		return state, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return state, errors.New("invalid edit journal")
	}
	data, err := os.ReadFile(j.Path)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}
