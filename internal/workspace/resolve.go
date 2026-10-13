package workspace

import "github.com/eaopen/cloudfile-local-agent/internal/config"

// Resolve returns the mirror root for this run, after making sure an existing
// mirror has followed the layout policy and that the on-disk layout is current.
//
// Every entry point that touches the mirror resolves the root through here, so
// the move onto another drive and the view/edit split happen exactly once,
// before the first file is written.
func Resolve(cfg config.Config) (string, error) {
	root, err := cfg.Root()
	if err != nil {
		return "", err
	}
	// An explicitly pinned workspace_root is the operator's decision: a mirror
	// is never relocated away from it.
	if cfg.WorkspaceRoot == "" {
		if fallback, fallbackErr := config.FallbackRoot(); fallbackErr == nil {
			if err := Migrate(fallback, root); err != nil {
				return "", err
			}
		}
	}
	if err := Ensure(root); err != nil {
		return "", err
	}
	return root, nil
}
