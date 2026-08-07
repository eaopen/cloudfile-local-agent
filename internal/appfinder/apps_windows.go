//go:build windows

package appfinder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func platformApplications() []Application {
	roots := programRoots()
	applications := make([]Application, 0, 12)
	appendFirst := func(id, name string, extensions, patterns []string) {
		for _, executable := range appPathNames[id] {
			if path := appPathExecutable(executable); path != "" {
				applications = append(applications, Application{ID: id, Name: name, Extensions: extensions, Command: []string{path, "{file}"}})
				return
			}
		}
		for _, pattern := range patterns {
			for _, path := range expandPatterns(roots, pattern) {
				if isExecutable(path) {
					applications = append(applications, Application{ID: id, Name: name, Extensions: extensions, Command: []string{path, "{file}"}})
					return
				}
			}
		}
	}

	appendFirst("word", "Microsoft Word", []string{"doc", "docx", "dot", "dotx", "rtf"}, []string{
		"Microsoft Office/root/Office*/WINWORD.EXE", "Microsoft Office/Office*/WINWORD.EXE"})
	appendFirst("excel", "Microsoft Excel", []string{"xls", "xlsx", "xlsm", "xlsb", "csv"}, []string{
		"Microsoft Office/root/Office*/EXCEL.EXE", "Microsoft Office/Office*/EXCEL.EXE"})
	appendFirst("powerpoint", "Microsoft PowerPoint", []string{"ppt", "pptx", "pptm", "ppsx"}, []string{
		"Microsoft Office/root/Office*/POWERPNT.EXE", "Microsoft Office/Office*/POWERPNT.EXE"})
	appendFirst("visio", "Microsoft Visio", []string{"vsd", "vsdx", "vssx"}, []string{
		"Microsoft Office/root/Office*/VISIO.EXE", "Microsoft Office/Office*/VISIO.EXE"})
	appendFirst("libreoffice", "LibreOffice", []string{"doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "ods", "odp", "csv", "rtf"}, []string{
		"LibreOffice/program/soffice.exe"})

	appendFirst("autocad", "AutoCAD", []string{"dwg", "dxf", "dwt"}, []string{"Autodesk/AutoCAD*/acad.exe"})
	appendFirst("bricscad", "BricsCAD", []string{"dwg", "dxf", "dwt"}, []string{"Bricsys/BricsCAD*/bricscad.exe"})
	appendFirst("draftsight", "DraftSight", []string{"dwg", "dxf", "dwt"}, []string{"Dassault Systemes/DraftSight*/bin/DraftSight.exe"})
	appendFirst("revit", "Autodesk Revit", []string{"rvt", "rfa", "rte", "rft"}, []string{"Autodesk/Revit*/Revit.exe"})
	appendFirst("solidworks", "SOLIDWORKS", []string{"sldprt", "sldasm", "slddrw"}, []string{"SOLIDWORKS Corp/SOLIDWORKS/SLDWORKS.exe"})
	appendFirst("creo", "PTC Creo", []string{"prt", "asm", "drw"}, []string{"PTC/Creo*/Common Files/bin/parametric.exe"})
	appendFirst("nx", "Siemens NX", []string{"prt", "asm"}, []string{"Siemens/NX*/NXBIN/ugraf.exe"})
	appendFirst("catia", "CATIA", []string{"catpart", "catproduct", "catdrawing"}, []string{"Dassault Systemes/B*/win_b64/code/bin/CNEXT.exe"})
	appendFirst("sketchup", "SketchUp", []string{"skp"}, []string{"SketchUp/SketchUp*/SketchUp.exe"})
	appendFirst("rhino", "Rhino", []string{"3dm"}, []string{"Rhino*/System/Rhino.exe"})
	appendFirst("freecad", "FreeCAD", []string{"step", "stp", "iges", "igs", "stl", "obj", "fcstd"}, []string{"FreeCAD*/bin/FreeCAD.exe"})
	return applications
}

var appPathNames = map[string][]string{
	"word":        {"WINWORD.EXE"},
	"excel":       {"EXCEL.EXE"},
	"powerpoint":  {"POWERPNT.EXE"},
	"visio":       {"VISIO.EXE"},
	"libreoffice": {"soffice.exe"},
	"autocad":     {"acad.exe"},
	"bricscad":    {"bricscad.exe"},
	"draftsight":  {"DraftSight.exe"},
	"revit":       {"Revit.exe"},
	"solidworks":  {"SLDWORKS.exe"},
	"creo":        {"parametric.exe"},
	"nx":          {"ugraf.exe"},
	"catia":       {"CNEXT.exe"},
	"sketchup":    {"SketchUp.exe"},
	"rhino":       {"Rhino.exe"},
	"freecad":     {"FreeCAD.exe"},
}

// App Paths is the documented Windows registration point for mapping an
// executable name to its absolute path.  It catches per-user installs that
// do not live under Program Files; a failed registry query merely falls back
// to the conservative known-directory scan below.
func appPathExecutable(executable string) string {
	keys := []string{
		`HKCU\Software\Microsoft\Windows\CurrentVersion\App Paths\` + executable,
		`HKLM\Software\Microsoft\Windows\CurrentVersion\App Paths\` + executable,
		`HKLM\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\App Paths\` + executable,
	}
	for _, key := range keys {
		output, err := exec.Command("reg.exe", "query", key, "/ve").Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(output), "\n") {
			for _, valueType := range []string{"REG_SZ", "REG_EXPAND_SZ"} {
				if index := strings.Index(line, valueType); index >= 0 {
					path := strings.TrimSpace(line[index+len(valueType):])
					path = os.ExpandEnv(path)
					if isExecutable(path) {
						return path
					}
				}
			}
		}
	}
	return ""
}

func programRoots() []string {
	values := []string{os.Getenv("ProgramW6432"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")}
	roots := make([]string, 0, len(values))
	seen := make(map[string]bool)
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			roots = append(roots, value)
		}
	}
	return roots
}

func expandPatterns(roots []string, pattern string) []string {
	paths := make([]string, 0)
	for _, root := range roots {
		matches, _ := filepath.Glob(filepath.Join(root, pattern))
		paths = append(paths, matches...)
	}
	return paths
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
