//go:build !windows && !darwin

package appfinder

import "os/exec"

func platformApplications() []Application {
	applications := make([]Application, 0, 3)
	appendPath := func(id, name string, extensions []string, executable string) {
		if path, err := exec.LookPath(executable); err == nil {
			applications = append(applications, Application{ID: id, Name: name, Extensions: extensions, Command: []string{path, "{file}"}})
		}
	}
	appendPath("libreoffice", "LibreOffice", []string{"doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "ods", "odp", "csv", "rtf"}, "libreoffice")
	appendPath("freecad", "FreeCAD", []string{"step", "stp", "iges", "igs", "stl", "obj", "fcstd"}, "freecad")
	appendPath("inkscape", "Inkscape", []string{"svg", "dxf", "pdf"}, "inkscape")
	return applications
}
