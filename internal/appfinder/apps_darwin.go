//go:build darwin

package appfinder

import (
	"os"
	"path/filepath"
)

func platformApplications() []Application {
	applications := make([]Application, 0, 8)
	appendBundle := func(id, name string, extensions []string, bundles ...string) {
		home, _ := os.UserHomeDir()
		candidates := append([]string(nil), bundles...)
		for _, bundle := range bundles {
			if home != "" && filepath.Dir(bundle) == "/Applications" {
				candidates = append(candidates, filepath.Join(home, "Applications", filepath.Base(bundle)))
			}
		}
		for _, bundle := range candidates {
			if _, err := os.Stat(bundle); err == nil {
				applications = append(applications, Application{ID: id, Name: name, Extensions: extensions, Command: []string{"/usr/bin/open", "-a", bundle, "{file}"}})
				return
			}
		}
	}
	appendBundle("word", "Microsoft Word", []string{"doc", "docx", "dot", "dotx", "rtf"}, "/Applications/Microsoft Word.app")
	appendBundle("excel", "Microsoft Excel", []string{"xls", "xlsx", "xlsm", "xlsb", "csv"}, "/Applications/Microsoft Excel.app")
	appendBundle("powerpoint", "Microsoft PowerPoint", []string{"ppt", "pptx", "pptm", "ppsx"}, "/Applications/Microsoft PowerPoint.app")
	appendBundle("libreoffice", "LibreOffice", []string{"doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "ods", "odp", "csv", "rtf"}, "/Applications/LibreOffice.app")
	appendBundle("autocad", "AutoCAD", []string{"dwg", "dxf", "dwt"}, "/Applications/Autodesk/AutoCAD 2026/AutoCAD.app", "/Applications/Autodesk/AutoCAD 2025/AutoCAD.app")
	appendBundle("sketchup", "SketchUp", []string{"skp"}, "/Applications/SketchUp 2025/SketchUp.app", "/Applications/SketchUp 2024/SketchUp.app")
	appendBundle("rhino", "Rhino", []string{"3dm"}, "/Applications/Rhino 8.app", "/Applications/Rhino 7.app")
	appendBundle("freecad", "FreeCAD", []string{"step", "stp", "iges", "igs", "stl", "obj", "fcstd"}, "/Applications/FreeCAD.app")
	appendBundle("preview", "Preview", []string{"pdf", "png", "jpg", "jpeg", "tiff"}, "/System/Applications/Preview.app")
	appendBundle("textedit", "TextEdit", []string{"txt"}, "/System/Applications/TextEdit.app")
	return applications
}
