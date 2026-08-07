// Package appfinder discovers a small, curated set of local productivity and
// design applications.  It never accepts an executable path from a remote
// session; discovered paths are used only after the local Agent has validated
// the CloudFile descriptor and applied user-owned override rules.
package appfinder

import (
	"path/filepath"
	"strings"
	"sync"
)

type Application struct {
	ID         string
	Name       string
	Extensions []string
	Command    []string
}

var (
	discoverOnce sync.Once
	discovered   []Application
)

func Installed() []Application {
	discoverOnce.Do(func() { discovered = platformApplications() })
	return append([]Application(nil), discovered...)
}

func Names() []string {
	applications := Installed()
	names := make([]string, 0, len(applications))
	for _, application := range applications {
		names = append(names, application.Name)
	}
	return names
}

func ForFile(name string) (Application, bool) {
	extension := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	return choose(extension, Installed())
}

func choose(extension string, applications []Application) (Application, bool) {
	for _, application := range applications {
		for _, supported := range application.Extensions {
			if supported == extension {
				return application, true
			}
		}
	}
	return Application{}, false
}
