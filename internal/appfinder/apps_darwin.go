//go:build darwin

package appfinder

import "os"

func platformApplications() []Application {
	applications := make([]Application, 0, 8)
	appendBundle := func(id, name string, extensions []string, bundles ...string) {
		for _, bundle := range bundles {
			if _, err := os.Stat(bundle); err == nil {
				applications = append(applications, Application{ID: id, Name: name, Extensions: extensions, Command: []string{"/usr/bin/open", "-a", name, "{file}"}})
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
	return applications
}
